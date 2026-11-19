package launcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/beshkenadze/cs-project/internal/cgroups"
	"github.com/beshkenadze/cs-project/internal/protocol"
	"golang.org/x/sys/unix"
)

// LinuxSystem is System in the VM container.
type LinuxSystem struct{}

func (LinuxSystem) Run(name string, args, env []string, stdin []byte) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.Bytes(), err
}

func (LinuxSystem) RunDetached(name string, args []string, log *os.File) error {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, log, log
	return cmd.Run()
}

func (LinuxSystem) MountOverlay(lower []string, upper, work, target string) error {
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", strings.Join(lower, ":"), upper, work)
	return unix.Mount("overlay", target, "overlay", 0, opts)
}

func (LinuxSystem) Unmount(target string) error {
	err := unix.Unmount(target, unix.MNT_DETACH)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

// NewNetNS makes a network namespace and keeps it with a bind mount at path.
func (LinuxSystem) NewNetNS(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	f.Close()
	errc := make(chan error, 1)
	go func() {
		// The thread is never unlocked, so it ends with this goroutine and its namespace does not
		// leak into other goroutines.
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			errc <- err
			return
		}
		errc <- unix.Mount(fmt.Sprintf("/proc/%d/task/%d/ns/net", os.Getpid(), unix.Gettid()), path, "", unix.MS_BIND, "")
	}()
	if err := <-errc; err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func (s LinuxSystem) DeleteNetNS(path string) error {
	s.Unmount(path)
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (LinuxSystem) JoinCgroup(leaf string) error { return cgroups.Join(leaf, os.Getpid()) }

func (LinuxSystem) SetMembersMemoryMax(bytes uint64) error {
	return cgroups.SetMemoryMax("members", bytes)
}

// Report tells the init process the pid of a member.
func (LinuxSystem) Report(id string, pid int) error {
	conn, err := net.Dial("unix", protocol.ControlSock)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(protocol.Request{Op: "started", ID: id, PID: pid}); err != nil {
		return err
	}
	var reply map[string]string
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return err
	}
	if reply["error"] != "" {
		return errors.New(reply["error"])
	}
	return nil
}
