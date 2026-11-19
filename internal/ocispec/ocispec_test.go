package ocispec

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/beshkenadze/cs-project/internal/protocol"
)

func rootfs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "etc"), 0o755)
	os.WriteFile(filepath.Join(dir, "etc/passwd"), []byte("root:x:0:0:root:/root:/bin/sh\nredis:x:999:1000::/home/redis:/sbin/nologin\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "etc/group"), []byte("root:x:0:\nredis:x:1000:\nextra:x:1001:redis,other\n"), 0o644)
	return dir
}

func TestResolveUser(t *testing.T) {
	root := rootfs(t)
	cases := []struct {
		in        string
		uid, gid  uint32
		groups    []uint32
		shouldErr bool
	}{
		{"", 0, 0, nil, false},
		{"redis", 999, 1000, []uint32{1001}, false},
		{"redis:root", 999, 0, []uint32{1001}, false},
		{"1234", 1234, 0, nil, false},
		{"1234:55", 1234, 55, nil, false},
		{"nobody-here", 0, 0, nil, true},
	}
	for _, c := range cases {
		uid, gid, groups, err := ResolveUser(root, c.in)
		if c.shouldErr {
			if err == nil {
				t.Fatalf("%q: no error", c.in)
			}
			continue
		}
		if err != nil || uid != c.uid || gid != c.gid || !reflect.DeepEqual(groups, c.groups) {
			t.Fatalf("%q: %d %d %v %v", c.in, uid, gid, groups, err)
		}
	}
}

func member() protocol.Member {
	return protocol.Member{
		ID: "abc", Hostname: "a1", Args: []string{"httpd", "-f"}, Env: []string{"A=1"}, Cwd: "/srv",
		Mounts:    []protocol.Mount{{Kind: "volume", Key: "v1", Target: "/data"}},
		MemoryMax: 64 << 20, OCIRuntime: "runc",
	}
}

func TestBuild(t *testing.T) {
	root := rootfs(t)
	spec, err := Build(member(), Paths{Rootfs: root, Hosts: "/m/hosts", Resolv: "/m/resolv.conf", Hostname: "/m/hostname", NetNS: "/ns/abc", Volumes: "/data/volumes"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Process.Terminal || spec.Process.Cwd != "/srv" || !reflect.DeepEqual(spec.Process.Args, []string{"httpd", "-f"}) {
		t.Fatalf("process %+v", spec.Process)
	}
	if !contains(spec.Process.Env, "A=1") || !contains(spec.Process.Env, "HOSTNAME=a1") || !hasPrefix(spec.Process.Env, "PATH=") {
		t.Fatalf("env %v", spec.Process.Env)
	}
	if contains(spec.Process.Capabilities.Bounding, "CAP_SYS_ADMIN") || !contains(spec.Process.Capabilities.Bounding, "CAP_CHOWN") {
		t.Fatalf("caps %v", spec.Process.Capabilities.Bounding)
	}
	if spec.Linux.CgroupsPath != "/members/abc" || spec.Linux.Resources.Memory == nil || *spec.Linux.Resources.Memory.Limit != 64<<20 {
		t.Fatalf("cgroup %s %+v", spec.Linux.CgroupsPath, spec.Linux.Resources)
	}
	var netns string
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == "network" {
			netns = ns.Path
		}
	}
	if netns != "/ns/abc" {
		t.Fatalf("network namespace %q", netns)
	}
	if !contains(spec.Linux.ReadonlyPaths, "/proc/sys") || !contains(spec.Linux.MaskedPaths, "/proc/kcore") {
		t.Fatal("default masked or read-only paths missing")
	}
	dests := map[string]string{}
	for _, m := range spec.Mounts {
		dests[m.Destination] = m.Source
	}
	if dests["/etc/hosts"] != "/m/hosts" || dests["/etc/resolv.conf"] != "/m/resolv.conf" || dests["/data"] != "/data/volumes/v1" {
		t.Fatalf("mounts %v", dests)
	}
}

func TestBuildRefusesCapability(t *testing.T) {
	m := member()
	m.CapAdd = []string{"SYS_ADMIN"}
	if _, err := Build(m, Paths{Rootfs: rootfs(t)}); err == nil {
		t.Fatal("SYS_ADMIN accepted")
	}
}

func TestBuildRefusesUnknownRuntimeAndMount(t *testing.T) {
	m := member()
	m.OCIRuntime = "runsc"
	if _, err := Build(m, Paths{Rootfs: rootfs(t)}); err == nil {
		t.Fatal("runsc accepted")
	}
	m = member()
	m.Mounts = []protocol.Mount{{Kind: "bind", Key: "x", Target: "/x"}}
	if _, err := Build(m, Paths{Rootfs: rootfs(t)}); err == nil {
		t.Fatal("bind accepted in slice 1")
	}
	m = member()
	m.Mounts = []protocol.Mount{{Kind: "volume", Key: "../x", Target: "/x"}}
	if _, err := Build(m, Paths{Rootfs: rootfs(t)}); err == nil {
		t.Fatal("volume key with a path accepted")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func hasPrefix(list []string, p string) bool {
	for _, v := range list {
		if len(v) >= len(p) && v[:len(p)] == p {
			return true
		}
	}
	return false
}
