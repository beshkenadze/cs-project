// Package daemon is the init process of the project VM: it reaps the members, keeps their state
// and sends numbered events.
package daemon

import (
	"sort"
	"sync"
	"time"

	"github.com/beshkenadze/cs-project/internal/protocol"
)

var subscriberBuffer = 4096

const unclaimedLifetime = 30 * time.Second

type member struct {
	pid        int
	running    bool
	exitCode   int
	startedAt  time.Time
	finishedAt time.Time
	// cleaning: the process exited and its cleanup runs. The member still shows as running.
	cleaning bool
	status   int
	exitedAt time.Time
}

type reaped struct {
	status int
	at     time.Time
}

// State is the member table. All methods are safe for concurrent use.
type State struct {
	// OnExit runs when the process of a member exits, with the lock held. The init process
	// starts the exit cleanup from it and returns true; the exit is then published at Cleaned.
	OnExit      func(id string) bool
	mu          sync.Mutex
	seq         uint64
	members     map[string]*member
	byPID       map[int]string
	unclaimed   map[int]reaped // exits reaped before the start client reported the pid
	subscribers []chan protocol.Event
}

func NewState() *State {
	return &State{members: map[string]*member{}, byPID: map[int]string{}, unclaimed: map[int]reaped{}}
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// Started records the init process of a member.
func (s *State) Started(id string, pid int, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.members[id]; ok && old.running {
		delete(s.byPID, old.pid)
	}
	s.members[id] = &member{pid: pid, running: true, startedAt: at}
	s.byPID[pid] = id
	s.emit(protocol.Event{Type: "start", ID: id, PID: pid, Time: stamp(at)})
	if r, ok := s.unclaimed[pid]; ok {
		delete(s.unclaimed, pid)
		// A pid is reused after some time; an old exit of another process must not end the member.
		if at.Sub(r.at) < unclaimedLifetime {
			s.exit(id, r.status, r.at)
		}
	}
}

// Reaped records a reaped process. A pid that is no member yet is kept for Started.
func (s *State) Reaped(pid, status int, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byPID[pid]
	if !ok {
		s.unclaimed[pid] = reaped{status, at}
		if len(s.unclaimed) > 1024 {
			for p := range s.unclaimed {
				delete(s.unclaimed, p)
				break
			}
		}
		return
	}
	s.exit(id, status, at)
}

func (s *State) exit(id string, status int, at time.Time) {
	m := s.members[id]
	delete(s.byPID, m.pid)
	if s.OnExit != nil && s.OnExit(id) {
		m.cleaning, m.status, m.exitedAt = true, status, at
		return
	}
	s.finish(id, m, status, at)
}

func (s *State) finish(id string, m *member, status int, at time.Time) {
	m.running, m.cleaning, m.exitCode, m.finishedAt = false, false, status, at
	s.emit(protocol.Event{Type: "exit", ID: id, PID: m.pid, Code: status, Time: stamp(at)})
}

// Cleaned publishes the exit of a member whose cleanup ended.
func (s *State) Cleaned(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.members[id]; ok && m.cleaning {
		s.finish(id, m, m.status, m.exitedAt)
	}
}

func (s *State) emit(e protocol.Event) {
	s.seq++
	e.Seq = s.seq
	kept := s.subscribers[:0]
	for _, ch := range s.subscribers {
		select {
		case ch <- e:
			kept = append(kept, ch)
		default:
			// A subscriber that does not read loses the stream, never one event: the stream ends
			// and the client subscribes again for a new snapshot.
			close(ch)
		}
	}
	s.subscribers = kept
}

// Subscribe gives the snapshot and the channel of later events in one step.
func (s *State) Subscribe() (protocol.Event, chan protocol.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan protocol.Event, subscriberBuffer)
	s.subscribers = append(s.subscribers, ch)
	snap := protocol.Event{Seq: s.seq, Type: "snapshot", Members: []protocol.MemberState{}}
	for id, m := range s.members {
		ms := protocol.MemberState{ID: id, Running: m.running, ExitCode: m.exitCode, StartedAt: stamp(m.startedAt), FinishedAt: stamp(m.finishedAt)}
		if m.running {
			ms.PID = m.pid
		}
		snap.Members = append(snap.Members, ms)
	}
	sort.Slice(snap.Members, func(i, j int) bool { return snap.Members[i].ID < snap.Members[j].ID })
	return snap, ch
}

// Unsubscribe removes a channel from Subscribe.
func (s *State) Unsubscribe(ch chan protocol.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.subscribers {
		if c == ch {
			s.subscribers = append(s.subscribers[:i], s.subscribers[i+1:]...)
			return
		}
	}
}

// RunningPIDs gives the init pids of the running members.
func (s *State) RunningPIDs() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for _, m := range s.members {
		if m.running && !m.cleaning {
			out = append(out, m.pid)
		}
	}
	sort.Ints(out)
	return out
}

// IsMemberPID reports whether pid is the init process of a running member.
func (s *State) IsMemberPID(pid int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.byPID[pid]
	return ok
}
