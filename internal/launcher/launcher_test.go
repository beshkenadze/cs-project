package launcher

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/beshkenadze/cs-project/internal/protocol"
	"github.com/beshkenadze/cs-project/internal/rootfs"
)

// fakeSystem records commands and stands in for mounts. An overlay mount is a symbolic link from
// the target to the upper directory: writes reach the upper directory as with a real overlay.
type fakeSystem struct {
	t        *testing.T
	runs     [][]string
	mounts   map[string][]string // target -> lower directories
	netns    map[string]bool
	cgroup   string
	runcPID  int
	detached [][]string
	// membersMemoryMax: the last memory.max of the members/ cgroup.
	membersMemoryMax uint64
}

func newFake(t *testing.T) *fakeSystem {
	return &fakeSystem{t: t, mounts: map[string][]string{}, netns: map[string]bool{}, runcPID: 4242}
}

func (f *fakeSystem) Run(name string, args, env []string, stdin []byte) ([]byte, error) {
	f.runs = append(f.runs, append([]string{name}, args...))
	if strings.HasPrefix(filepath.Base(name), "bridge") || filepath.Base(name) == "loopback" {
		return []byte(`{"cniVersion":"1.0.0"}`), nil
	}
	return nil, nil
}

func (f *fakeSystem) RunDetached(name string, args []string, log *os.File) error {
	f.detached = append(f.detached, append([]string{name}, args...))
	for i, a := range args {
		if a == "--pid-file" {
			return os.WriteFile(args[i+1], []byte("4242"), 0o644)
		}
	}
	return nil
}

func (f *fakeSystem) MountOverlay(lower []string, upper, work, target string) error {
	os.RemoveAll(target)
	f.mounts[target] = lower
	return os.Symlink(upper, target)
}

func (f *fakeSystem) Unmount(target string) error {
	if _, ok := f.mounts[target]; !ok {
		return nil
	}
	delete(f.mounts, target)
	os.Remove(target)
	return os.MkdirAll(target, 0o755)
}

func (f *fakeSystem) NewNetNS(path string) error {
	f.netns[path] = true
	return os.WriteFile(path, nil, 0o644)
}

func (f *fakeSystem) DeleteNetNS(path string) error {
	delete(f.netns, path)
	return os.RemoveAll(path)
}

func (f *fakeSystem) JoinCgroup(leaf string) error {
	f.cgroup = leaf
	return nil
}

func (f *fakeSystem) Report(id string, pid int) error { return nil }

func (f *fakeSystem) SetMembersMemoryMax(bytes uint64) error {
	f.membersMemoryMax = bytes
	return nil
}

func tarLayer(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), Uid: os.Getuid(), Gid: os.Getgid()})
		tw.Write([]byte(body))
	}
	tw.Close()
	return buf.Bytes()
}

type env struct {
	l    *Launcher
	sys  *fakeSystem
	spec protocol.Member
}

func setup(t *testing.T) env {
	base := t.TempDir()
	sys := newFake(t)
	l := &Launcher{Sys: sys, Data: filepath.Join(base, "data"), Host: filepath.Join(base, "host"), State: filepath.Join(base, "state"),
		ResolvSource: filepath.Join(base, "resolv.conf")}
	os.WriteFile(l.ResolvSource, []byte("nameserver 192.168.64.1\n"), 0o644)
	var diffs, blobs []string
	for _, files := range []map[string]string{{"etc/passwd": "root:x:0:0::/root:/bin/sh\n", "bin/a": "1"}, {"bin/b": "2"}} {
		data := tarLayer(t, files)
		sum := sha256.Sum256(data)
		d := "sha256:" + hex.EncodeToString(sum[:])
		os.MkdirAll(filepath.Join(l.Host, "blobs/sha256"), 0o755)
		os.WriteFile(filepath.Join(l.Host, "blobs/sha256", hex.EncodeToString(sum[:])), data, 0o644)
		diffs = append(diffs, d)
		blobs = append(blobs, d)
	}
	spec := protocol.Member{
		ID: "m1", Hostname: "m1", Args: []string{"/bin/a"}, DiffIDs: diffs, Blobs: blobs, OCIRuntime: "runc",
		Mounts:   []protocol.Mount{{Kind: "volume", Key: "vol1", Target: "/var/lib/x"}},
		Networks: []protocol.Network{{Name: "p_default", Bridge: "csbr0", Address: "10.213.0.11/24", Gateway: "10.213.0.1", Interface: "eth0"}},
		Hosts:    "127.0.0.1\tlocalhost\n",
	}
	return env{l, sys, spec}
}

func TestCreateUnpacksChainInUnpackCgroup(t *testing.T) {
	e := setup(t)
	if err := e.l.Create(e.spec); err != nil {
		t.Fatal(err)
	}
	if e.sys.cgroup != "unpack" {
		t.Fatalf("unpack ran in cgroup %q", e.sys.cgroup)
	}
	chains := rootfs.ChainIDs(e.spec.DiffIDs)
	first := filepath.Join(e.l.Data, "layers", strings.TrimPrefix(chains[0], "sha256:"))
	second := filepath.Join(e.l.Data, "layers", strings.TrimPrefix(chains[1], "sha256:"))
	if _, err := os.Stat(filepath.Join(first, "bin/a")); err != nil {
		t.Fatal("first layer not unpacked")
	}
	if _, err := os.Stat(filepath.Join(second, "bin/b")); err != nil {
		t.Fatal("second layer not in its chain directory")
	}
	if _, err := os.Stat(filepath.Join(second, "bin/a")); err == nil {
		t.Fatal("second chain directory holds a copy of the parent")
	}
	if _, err := os.Stat(filepath.Join(e.l.Data, "volumes/vol1")); err != nil {
		t.Fatal("anonymous volume directory missing")
	}
	if b, _ := os.ReadFile(filepath.Join(e.l.Data, "members/m1/hosts")); string(b) != e.spec.Hosts {
		t.Fatalf("hosts %q", b)
	}
	// A second create of the same chain unpacks nothing.
	if err := os.WriteFile(filepath.Join(second, "marker"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.l.Create(e.spec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(second, "marker")); err != nil {
		t.Fatal("the chain directory was unpacked again")
	}
}

func TestStartMountsChainTopFirstAndRunsRunc(t *testing.T) {
	e := setup(t)
	e.l.Create(e.spec)
	pid, err := e.l.Start("m1")
	if err != nil {
		t.Fatal(err)
	}
	if pid != 4242 {
		t.Fatalf("pid %d", pid)
	}
	chains := rootfs.ChainIDs(e.spec.DiffIDs)
	lower := e.sys.mounts[filepath.Join(e.l.Data, "members/m1/rootfs")]
	want := []string{
		filepath.Join(e.l.Data, "layers", strings.TrimPrefix(chains[1], "sha256:")),
		filepath.Join(e.l.Data, "layers", strings.TrimPrefix(chains[0], "sha256:")),
	}
	if !reflect.DeepEqual(lower, want) {
		t.Fatalf("lower %v want %v", lower, want)
	}
	if !e.sys.netns[filepath.Join(e.l.State, "netns/m1")] {
		t.Fatal("no network namespace")
	}
	var cfg map[string]any
	b, err := os.ReadFile(filepath.Join(e.l.Data, "members/m1/config.json"))
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		t.Fatal("no config.json")
	}
	if len(e.sys.detached) != 1 || !contains(e.sys.detached[0], "--detach") || e.sys.detached[0][0] != "/bin/runc" {
		t.Fatalf("runc %v", e.sys.detached)
	}
	if !ranPlugin(e.sys.runs, "bridge") || !ranPlugin(e.sys.runs, "loopback") {
		t.Fatalf("CNI not run: %v", e.sys.runs)
	}
}

func TestExitCleanupKeepsWritableLayer(t *testing.T) {
	e := setup(t)
	e.l.Create(e.spec)
	e.l.Start("m1")
	upper := filepath.Join(e.l.Data, "members/m1/upper")
	os.WriteFile(filepath.Join(upper, "marker"), []byte("x"), 0o644)
	if err := e.l.Cleanup("m1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(upper, "marker")); err != nil {
		t.Fatal("exit cleanup deleted the writable layer")
	}
	if len(e.sys.mounts) != 0 || len(e.sys.netns) != 0 {
		t.Fatal("exit cleanup left a mount or a network namespace")
	}
	if !ranArg(e.sys.runs, "delete") {
		t.Fatal("runc state not deleted")
	}
	// A second start uses the same writable layer.
	if _, err := e.l.Start("m1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.l.Data, "members/m1/rootfs/marker")); err != nil {
		t.Fatal("the second start does not see the writable layer")
	}
}

func TestRemoveDeletesWritableLayerAndVolumes(t *testing.T) {
	e := setup(t)
	e.l.Create(e.spec)
	e.l.Start("m1")
	if err := e.l.Remove(protocol.Remove{ID: "m1", Volumes: []string{"vol1"}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"members/m1", "volumes/vol1"} {
		if _, err := os.Stat(filepath.Join(e.l.Data, p)); err == nil {
			t.Fatalf("%s still exists", p)
		}
	}
	if len(e.sys.mounts) != 0 {
		t.Fatal("remove left a mount")
	}
}

func TestBootDeletesUnknownMembersVolumesAndLayers(t *testing.T) {
	e := setup(t)
	e.l.Create(e.spec)
	other := e.spec
	other.ID = "m2"
	other.DiffIDs = e.spec.DiffIDs[:1]
	other.Blobs = e.spec.Blobs[:1]
	other.Mounts = []protocol.Mount{{Kind: "volume", Key: "vol2", Target: "/x"}}
	e.l.Create(other)
	if err := e.l.Boot(protocol.Boot{Keep: []string{"m2"}, Volumes: []string{"vol2"}}); err != nil {
		t.Fatal(err)
	}
	chains := rootfs.ChainIDs(e.spec.DiffIDs)
	checks := map[string]bool{
		"members/m1": false, "members/m2": true, "volumes/vol1": false, "volumes/vol2": true,
		"layers/" + strings.TrimPrefix(chains[0], "sha256:"): true,
		"layers/" + strings.TrimPrefix(chains[1], "sha256:"): false,
	}
	for p, want := range checks {
		_, err := os.Stat(filepath.Join(e.l.Data, p))
		if (err == nil) != want {
			t.Fatalf("%s exists=%v, want %v", p, err == nil, want)
		}
	}
}

func TestWriteHostsKeepsInode(t *testing.T) {
	e := setup(t)
	e.l.Create(e.spec)
	p := filepath.Join(e.l.Data, "members/m1/hosts")
	before, _ := os.Stat(p)
	if err := e.l.WriteHosts(map[string]string{"m1": "10.213.0.12\tb\n", "unknown": "x"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(p)
	if !os.SameFile(before, after) {
		t.Fatal("hosts file replaced, not rewritten")
	}
	if b, _ := os.ReadFile(p); string(b) != "10.213.0.12\tb\n" {
		t.Fatalf("hosts %q", b)
	}
}

func TestRefusesInvalidID(t *testing.T) {
	e := setup(t)
	e.spec.ID = "../x"
	if err := e.l.Create(e.spec); err == nil {
		t.Fatal("accepted an ID with a path")
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

func ranPlugin(runs [][]string, plugin string) bool {
	for _, r := range runs {
		if filepath.Base(r[0]) == plugin {
			return true
		}
	}
	return false
}

func ranArg(runs [][]string, arg string) bool {
	for _, r := range runs {
		if contains(r, arg) {
			return true
		}
	}
	return false
}

// Start takes the member lock, as the exit cleanup does, so the two never overlap.
func TestStartWaitsForTheMemberLock(t *testing.T) {
	e := setup(t)
	e.l.Create(e.spec)
	unlock, err := e.l.lockMember("m1")
	if err != nil {
		t.Fatal(err)
	}
	// Set after the test holds the lock: it reports the next caller that waits for it.
	arrived := make(chan struct{})
	e.l.lockWaiting = func(id string) { close(arrived) }
	done := make(chan error, 1)
	go func() {
		_, err := e.l.Start("m1")
		done <- err
	}()
	select {
	case <-arrived:
	case err := <-done:
		t.Fatalf("start ended without the member lock: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("start did not wait for the member lock")
	}
	if len(e.sys.mounts) != 0 || len(e.sys.detached) != 0 {
		t.Fatal("start worked while the cleanup held the member lock")
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The members/ cgroup gets the VM memory minus the reserve of the launcher, so a member without
// a limit cannot take the memory of the launcher.
func TestBootLimitsTheMembersCgroup(t *testing.T) {
	e := setup(t)
	if err := e.l.Boot(protocol.Boot{MembersMemoryMax: 3840 << 20}); err != nil {
		t.Fatal(err)
	}
	if e.sys.membersMemoryMax != 3840<<20 {
		t.Fatalf("members memory.max %d", e.sys.membersMemoryMax)
	}
}
