// Package cgroups makes the cgroup layout of the project VM. cgroup v2 forbids processes in a
// cgroup that gives controllers to its children, so each process is in a leaf:
//
//	/                subtree_control: +cpu +memory +pids
//	├── launcher/    the launcher and its exec clients
//	├── unpack/      layer unpack, memory.max
//	└── members/     no process; subtree_control: +cpu +memory +pids
//	    └── <id>/    one member (made by runc)
package cgroups

import (
	"os"
	"path/filepath"
	"strconv"
)

const (
	Root        = "/sys/fs/cgroup"
	controllers = "+cpu +memory +pids"
)

// Setup moves pid into launcher/ and makes the layout under Root.
func Setup(pid int, unpackMemoryMax int64) error {
	return setup(Root, pid, unpackMemoryMax, write)
}

// Join moves pid into the leaf name, for example "unpack".
func Join(leaf string, pid int) error {
	return write(filepath.Join(Root, leaf, "cgroup.procs"), strconv.Itoa(pid))
}

// SetMemoryMax writes memory.max of the cgroup leaf under Root.
func SetMemoryMax(leaf string, bytes uint64) error {
	return write(filepath.Join(Root, leaf, "memory.max"), strconv.FormatUint(bytes, 10))
}

// MemberPath is the OCI cgroupsPath of a member.
func MemberPath(id string) string { return "/members/" + id }

func setup(root string, pid int, unpackMemoryMax int64, w func(string, string) error) error {
	for _, dir := range []string{"launcher", "unpack", "members"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return err
		}
	}
	steps := [][2]string{
		{"launcher/cgroup.procs", strconv.Itoa(pid)},
		{"cgroup.subtree_control", controllers},
		{"unpack/memory.max", strconv.FormatInt(unpackMemoryMax, 10)},
		{"members/cgroup.subtree_control", controllers},
	}
	for _, s := range steps {
		if err := w(filepath.Join(root, s[0]), s[1]); err != nil {
			return err
		}
	}
	return nil
}

func write(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(value)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
