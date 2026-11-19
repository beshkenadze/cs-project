package daemon

import (
	"testing"
	"time"

	"github.com/beshkenadze/cs-project/internal/protocol"
)

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func TestStartExitEventsAreNumbered(t *testing.T) {
	s := NewState()
	snap, ch := s.Subscribe()
	if snap.Type != "snapshot" || snap.Seq != 0 || len(snap.Members) != 0 {
		t.Fatalf("snapshot %+v", snap)
	}
	s.Started("a", 10, t0)
	s.Reaped(10, 3, t0.Add(time.Second))
	start := <-ch
	exit := <-ch
	if start.Type != "start" || start.Seq != 1 || start.ID != "a" || start.PID != 10 {
		t.Fatalf("start %+v", start)
	}
	if exit.Type != "exit" || exit.Seq != 2 || exit.ID != "a" || exit.Code != 3 {
		t.Fatalf("exit %+v", exit)
	}
}

func TestSnapshotAndSubscriptionAreOneOperation(t *testing.T) {
	s := NewState()
	s.Started("a", 10, t0)
	s.Started("b", 11, t0)
	s.Reaped(10, 0, t0)
	snap, ch := s.Subscribe()
	if snap.Seq != 3 {
		t.Fatalf("snapshot seq %d", snap.Seq)
	}
	got := map[string]protocol.MemberState{}
	for _, m := range snap.Members {
		got[m.ID] = m
	}
	if got["a"].Running || got["a"].FinishedAt == "" || !got["b"].Running || got["b"].PID != 11 {
		t.Fatalf("members %+v", got)
	}
	s.Reaped(11, 137, t0)
	ev := <-ch
	if ev.Seq != 4 || ev.ID != "b" || ev.Code != 137 {
		t.Fatalf("event after snapshot %+v", ev)
	}
}

// An exit can be reaped before the start client reports the pid.
func TestExitBeforeStartedIsKept(t *testing.T) {
	s := NewState()
	s.Reaped(20, 1, t0)
	s.Started("c", 20, t0)
	snap, _ := s.Subscribe()
	if len(snap.Members) != 1 || snap.Members[0].Running || snap.Members[0].ExitCode != 1 {
		t.Fatalf("members %+v", snap.Members)
	}
}

func TestUnknownPIDIsIgnored(t *testing.T) {
	s := NewState()
	s.Reaped(99, 0, t0)
	s.Started("d", 30, t0)
	snap, _ := s.Subscribe()
	if !snap.Members[0].Running {
		t.Fatal("an unrelated exit ended the member")
	}
}

func TestRunningPIDs(t *testing.T) {
	s := NewState()
	s.Started("a", 10, t0)
	s.Started("b", 11, t0)
	s.Reaped(10, 0, t0)
	if pids := s.RunningPIDs(); len(pids) != 1 || pids[0] != 11 {
		t.Fatalf("pids %v", pids)
	}
}

func TestSlowSubscriberStreamEnds(t *testing.T) {
	old := subscriberBuffer
	subscriberBuffer = 1
	defer func() { subscriberBuffer = old }()
	s := NewState()
	_, ch := s.Subscribe()
	s.Started("a", 1, t0)
	s.Started("b", 2, t0)
	<-ch
	if _, ok := <-ch; ok {
		t.Fatal("the stream did not end after an event was not delivered")
	}
}

func TestOldUnclaimedExitDoesNotEndMember(t *testing.T) {
	s := NewState()
	s.Reaped(40, 1, t0)
	s.Started("e", 40, t0.Add(time.Minute))
	snap, _ := s.Subscribe()
	if !snap.Members[0].Running {
		t.Fatal("an exit of a reused pid ended the member")
	}
}

func TestOnExitRunsForEachExit(t *testing.T) {
	s := NewState()
	var ended []string
	s.OnExit = func(id string) { ended = append(ended, id) }
	s.Reaped(50, 0, t0)
	s.Started("early", 50, t0)
	s.Started("late", 51, t0)
	s.Reaped(51, 0, t0)
	if len(ended) != 2 || ended[0] != "early" || ended[1] != "late" {
		t.Fatalf("ended %v", ended)
	}
}
