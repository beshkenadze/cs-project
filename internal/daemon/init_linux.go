package daemon

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"github.com/beshkenadze/cs-project/internal/cgroups"
	"github.com/beshkenadze/cs-project/internal/protocol"
	"golang.org/x/sys/unix"
)

const (
	unpackMemoryMax = 256 << 20
	stopGrace       = 3 * time.Second
)

// helpers are the exit-cleanup processes that the init process starts. The reaper skips them.
type helpers struct {
	mu   sync.Mutex
	pids map[int]bool
}

// RunInit is the init process of the VM container.
func RunInit() error {
	if err := os.MkdirAll(protocol.StateDir, 0o700); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", protocol.StateDir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0700"); err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return err
	}
	if err := cgroups.Setup(os.Getpid(), unpackMemoryMax); err != nil {
		return err
	}
	signals := make(chan os.Signal, 64)
	signal.Notify(signals, unix.SIGCHLD, unix.SIGTERM, unix.SIGINT)

	h := &helpers{pids: map[int]bool{}}
	state := NewState()
	state.OnExit = func(id string) { h.startCleanup(id) }

	ln, err := net.Listen("unix", protocol.ControlSock)
	if err != nil {
		return err
	}
	if err := os.Chmod(protocol.ControlSock, 0o600); err != nil {
		return err
	}
	go serve(ln, state)

	for s := range signals {
		switch s {
		case unix.SIGCHLD:
			reap(state, h)
		default:
			shutdown(state, h)
			return nil
		}
	}
	return nil
}

func (h *helpers) startCleanup(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out, err := os.OpenFile(filepath.Join(protocol.StateDir, "cleanup-"+id+".txt"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	defer out.Close()
	cmd := exec.Command("/proc/self/exe", "cleanup", id)
	cmd.Stdout, cmd.Stderr = out, out
	if cmd.Start() == nil {
		h.pids[cmd.Process.Pid] = true
	}
}

func reap(state *State, h *helpers) {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
		h.mu.Lock()
		helper := h.pids[pid]
		delete(h.pids, pid)
		h.mu.Unlock()
		if helper {
			continue
		}
		code := ws.ExitStatus()
		if ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
		state.Reaped(pid, code, time.Now())
	}
}

// shutdown runs when the VM container stops: members get SIGTERM, then SIGKILL after stopGrace.
func shutdown(state *State, h *helpers) {
	for _, pid := range state.RunningPIDs() {
		unix.Kill(pid, unix.SIGTERM)
	}
	deadline := time.Now().Add(stopGrace)
	for time.Now().Before(deadline) && len(state.RunningPIDs()) > 0 {
		time.Sleep(100 * time.Millisecond)
		reap(state, h)
	}
	for _, pid := range state.RunningPIDs() {
		unix.Kill(pid, unix.SIGKILL)
	}
}

func serve(ln net.Listener, state *State) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handle(conn, state)
	}
}

func handle(conn net.Conn, state *State) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	enc := json.NewEncoder(conn)
	var req protocol.Request
	if err := json.NewDecoder(r).Decode(&req); err != nil {
		return
	}
	switch req.Op {
	case "started":
		state.Started(req.ID, req.PID, time.Now())
		enc.Encode(map[string]string{"ok": "1"})
	case "subscribe":
		snap, ch := state.Subscribe()
		defer state.Unsubscribe(ch)
		if enc.Encode(snap) != nil {
			return
		}
		gone := make(chan struct{})
		go func() {
			buf := make([]byte, 1)
			conn.Read(buf)
			close(gone)
		}()
		for {
			select {
			case e, ok := <-ch:
				if !ok || enc.Encode(e) != nil {
					return
				}
			case <-gone:
				return
			}
		}
	default:
		enc.Encode(map[string]string{"error": "unknown op " + req.Op})
	}
}
