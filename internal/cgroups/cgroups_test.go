package cgroups

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestSetupPutsProcessesInLeavesOnly(t *testing.T) {
	root := t.TempDir()
	var order []string
	w := func(p, v string) error {
		order = append(order, strings.TrimPrefix(p, root+"/"))
		return os.WriteFile(p, []byte(v), 0o644)
	}
	if err := setup(root, 42, 256<<20, w); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(root, "launcher/cgroup.procs")); got != "42" {
		t.Fatalf("launcher procs %q", got)
	}
	if got := read(t, filepath.Join(root, "cgroup.subtree_control")); got != "+cpu +memory +pids" {
		t.Fatalf("root subtree %q", got)
	}
	if got := read(t, filepath.Join(root, "unpack/memory.max")); got != "268435456" {
		t.Fatalf("unpack memory.max %q", got)
	}
	if got := read(t, filepath.Join(root, "members/cgroup.subtree_control")); got != "+cpu +memory +pids" {
		t.Fatalf("members subtree %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "members/cgroup.procs")); err == nil {
		t.Fatal("a process was put into the members parent")
	}
	// The no-internal-process rule: the process leaves the root before the root gives controllers.
	if order[0] != "launcher/cgroup.procs" || order[1] != "cgroup.subtree_control" {
		t.Fatalf("order %v", order)
	}
}

func TestMemberPath(t *testing.T) {
	if got := MemberPath("abc"); got != "/members/abc" {
		t.Fatal(got)
	}
}
