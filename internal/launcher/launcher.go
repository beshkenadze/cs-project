// Package launcher has the member operations of the exec clients: create (unpack), start, exit
// cleanup, remove, boot and the /etc/hosts rewrite. System has the Linux-only parts.
package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/beshkenadze/cs-project/internal/ocispec"
	"github.com/beshkenadze/cs-project/internal/protocol"
	"github.com/beshkenadze/cs-project/internal/rootfs"
	"golang.org/x/sys/unix"
)

// CNIDir has the CNI plugins in the image.
const CNIDir = "/opt/cni/bin"

// System is the part that needs Linux and root.
type System interface {
	Run(name string, args, env []string, stdin []byte) ([]byte, error)
	RunDetached(name string, args []string, log *os.File) error
	MountOverlay(lower []string, upper, work, target string) error
	Unmount(target string) error
	NewNetNS(path string) error
	DeleteNetNS(path string) error
	JoinCgroup(leaf string) error
	Report(id string, pid int) error
	// SetMembersMemoryMax writes memory.max of the members/ cgroup.
	SetMembersMemoryMax(bytes uint64) error
}

// Launcher works on the directories of one project VM.
type Launcher struct {
	Sys          System
	Data         string // the data volume
	Host         string // the host share: blobs and logs
	State        string // tmpfs: runc state, network namespaces, locks
	ResolvSource string // /etc/resolv.conf of the VM
	// lockWaiting, when set, runs before a caller waits for a member lock. Tests use it.
	lockWaiting func(id string)
}

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

func checkName(kind, name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid %s %q", kind, name)
	}
	return nil
}

func (l *Launcher) memberDir(id string) string { return filepath.Join(l.Data, "members", id) }
func (l *Launcher) netnsPath(id string) string { return filepath.Join(l.State, "netns", id) }
func (l *Launcher) runcRoot() string           { return filepath.Join(l.State, "runc") }

func (l *Launcher) layerDir(chain string) string {
	return filepath.Join(l.Data, "layers", strings.TrimPrefix(chain, "sha256:"))
}

func (l *Launcher) readSpec(id string) (protocol.Member, error) {
	var m protocol.Member
	b, err := os.ReadFile(filepath.Join(l.memberDir(id), "spec.json"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func validate(m protocol.Member) error {
	if err := checkName("member ID", m.ID); err != nil {
		return err
	}
	if len(m.DiffIDs) == 0 || len(m.DiffIDs) != len(m.Blobs) {
		return errors.New("diffIDs and blobs must have the same, non-zero length")
	}
	for _, d := range append(append([]string{}, m.DiffIDs...), m.Blobs...) {
		if err := checkName("digest", strings.TrimPrefix(d, "sha256:")); err != nil || !strings.HasPrefix(d, "sha256:") {
			return fmt.Errorf("invalid digest %q", d)
		}
	}
	for _, mt := range m.Mounts {
		if err := checkName("volume key", mt.Key); err != nil {
			return err
		}
	}
	if _, ok := ocispec.Runtimes[m.OCIRuntime]; !ok {
		return fmt.Errorf("unknown OCI runtime %q", m.OCIRuntime)
	}
	return nil
}

// Create unpacks the layers of a member and prepares its directory. It runs in the unpack
// cgroup, so the page cache of the unpack stays under its memory limit.
func (l *Launcher) Create(m protocol.Member) error {
	if err := validate(m); err != nil {
		return err
	}
	if err := l.Sys.JoinCgroup("unpack"); err != nil {
		return fmt.Errorf("join the unpack cgroup: %w", err)
	}
	dir := l.memberDir(m.ID)
	for _, d := range []string{"upper", "work", "rootfs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return err
		}
	}
	if err := l.unpack(m); err != nil {
		return err
	}
	for _, mt := range m.Mounts {
		if err := os.MkdirAll(filepath.Join(l.Data, "volumes", mt.Key), 0o755); err != nil {
			return err
		}
	}
	resolv, err := os.ReadFile(l.ResolvSource)
	if err != nil {
		resolv = nil
	}
	files := map[string][]byte{"hosts": []byte(m.Hosts), "resolv.conf": resolv, "hostname": []byte(m.Hostname + "\n")}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	spec, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "spec.json"), spec, 0o600)
}

func (l *Launcher) unpack(m protocol.Member) error {
	layers := filepath.Join(l.Data, "layers")
	if err := os.MkdirAll(layers, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(layers, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	chains := rootfs.ChainIDs(m.DiffIDs)
	for i, chain := range chains {
		dst := l.layerDir(chain)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := l.unpackOne(m.Blobs[i], chains[:i], dst); err != nil {
			return fmt.Errorf("unpack layer %d (%s): %w", i, m.DiffIDs[i], err)
		}
	}
	return nil
}

// unpackOne applies one blob to an overlay of its parent chain and keeps the upper directory.
func (l *Launcher) unpackOne(blob string, parents []string, dst string) error {
	tmp := dst + ".tmp-" + strconv.Itoa(os.Getpid())
	os.RemoveAll(tmp)
	defer os.RemoveAll(tmp)
	upper := filepath.Join(tmp, "upper")
	if err := os.MkdirAll(upper, 0o755); err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(l.Host, "blobs/sha256", strings.TrimPrefix(blob, "sha256:")))
	if err != nil {
		return err
	}
	defer f.Close()
	target := upper
	if len(parents) > 0 {
		work, merged := filepath.Join(tmp, "work"), filepath.Join(tmp, "merged")
		for _, d := range []string{work, merged} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return err
			}
		}
		if err := l.Sys.MountOverlay(l.lowerDirs(parents), upper, work, merged); err != nil {
			return err
		}
		defer l.Sys.Unmount(merged)
		target = merged
	}
	if err := rootfs.Apply(target, f); err != nil {
		return err
	}
	if target != upper {
		if err := l.Sys.Unmount(target); err != nil {
			return err
		}
	}
	return os.Rename(upper, dst)
}

// lowerDirs gives the overlay lower directories of a chain, top layer first.
func (l *Launcher) lowerDirs(chains []string) []string {
	out := make([]string, 0, len(chains))
	for i := len(chains) - 1; i >= 0; i-- {
		out = append(out, l.layerDir(chains[i]))
	}
	return out
}

func cniConfig(n protocol.Network, defaultRoute bool) []byte {
	conf := map[string]any{
		"cniVersion": "1.0.0", "name": n.Name, "type": "bridge", "bridge": n.Bridge,
		"isGateway": true, "ipMasq": false, "hairpinMode": true,
	}
	ipam := map[string]any{"type": "static", "addresses": []map[string]string{{"address": n.Address, "gateway": n.Gateway}}}
	if defaultRoute {
		ipam["routes"] = []map[string]string{{"dst": "0.0.0.0/0", "gw": n.Gateway}}
	}
	conf["ipam"] = ipam
	b, _ := json.Marshal(conf)
	return b
}

var loopbackConfig = []byte(`{"cniVersion":"1.0.0","name":"loopback","type":"loopback"}`)

func (l *Launcher) cni(command, id string, plugin, ifname string, conf []byte) error {
	env := []string{"CNI_COMMAND=" + command, "CNI_CONTAINERID=" + id, "CNI_NETNS=" + l.netnsPath(id),
		"CNI_IFNAME=" + ifname, "CNI_PATH=" + CNIDir}
	out, err := l.Sys.Run(filepath.Join(CNIDir, plugin), nil, env, conf)
	if err != nil {
		return fmt.Errorf("CNI %s %s %s: %w: %s", command, plugin, ifname, err, out)
	}
	return nil
}

// Start mounts the rootfs, connects the networks and runs the member with runc. The stdout and
// stderr of the member go to its log file on the host share.
func (l *Launcher) Start(id string) (pid int, err error) {
	if err := checkName("member ID", id); err != nil {
		return 0, err
	}
	// The exit cleanup of an earlier run takes the same lock.
	unlock, err := l.lockMember(id)
	if err != nil {
		return 0, err
	}
	defer unlock()
	m, err := l.readSpec(id)
	if err != nil {
		return 0, err
	}
	dir := l.memberDir(id)
	rootDir := filepath.Join(dir, "rootfs")
	l.Sys.Unmount(rootDir)
	l.Sys.DeleteNetNS(l.netnsPath(id))
	defer func() {
		if err != nil {
			l.cleanup(id)
		}
	}()
	if err := l.Sys.MountOverlay(l.lowerDirs(rootfs.ChainIDs(m.DiffIDs)), filepath.Join(dir, "upper"), filepath.Join(dir, "work"), rootDir); err != nil {
		return 0, fmt.Errorf("mount the rootfs: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(l.netnsPath(id)), 0o755); err != nil {
		return 0, err
	}
	if err := l.Sys.NewNetNS(l.netnsPath(id)); err != nil {
		return 0, fmt.Errorf("network namespace: %w", err)
	}
	if err := l.cni("ADD", id, "loopback", "lo", loopbackConfig); err != nil {
		return 0, err
	}
	for i, n := range m.Networks {
		if err := l.cni("ADD", id, "bridge", n.Interface, cniConfig(n, i == 0)); err != nil {
			return 0, err
		}
	}
	spec, err := ocispec.Build(m, ocispec.Paths{
		Rootfs: rootDir, Hosts: filepath.Join(dir, "hosts"), Resolv: filepath.Join(dir, "resolv.conf"),
		Hostname: filepath.Join(dir, "hostname"), NetNS: l.netnsPath(id), Volumes: filepath.Join(l.Data, "volumes"),
	})
	if err != nil {
		return 0, err
	}
	config, err := json.Marshal(spec)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), config, 0o600); err != nil {
		return 0, err
	}
	logs := filepath.Join(l.Host, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		return 0, err
	}
	log, err := os.OpenFile(filepath.Join(logs, id+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer log.Close()
	pidFile := filepath.Join(dir, "pid")
	os.Remove(pidFile)
	runtime := ocispec.Runtimes[m.OCIRuntime]
	if err := l.Sys.RunDetached(runtime, []string{"--root", l.runcRoot(), "run", "--detach", "--pid-file", pidFile, "--bundle", dir, id}, log); err != nil {
		return 0, fmt.Errorf("runc run: %w", err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, err
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, err
	}
	if err := l.Sys.Report(id, pid); err != nil {
		return 0, fmt.Errorf("report the start: %w", err)
	}
	return pid, nil
}

// Kill sends a signal to the init process of a member.
func (l *Launcher) Kill(id, signal string) error {
	if err := checkName("member ID", id); err != nil {
		return err
	}
	out, err := l.Sys.Run(ocispec.Runtimes["runc"], []string{"--root", l.runcRoot(), "kill", id, signal}, nil, nil)
	if err != nil {
		return fmt.Errorf("runc kill: %w: %s", err, out)
	}
	return nil
}

// Cleanup runs after the init process of a member exits: it removes the runc state, the networks
// and the rootfs mount. The writable layer stays for a later start.
func (l *Launcher) Cleanup(id string) error {
	if err := checkName("member ID", id); err != nil {
		return err
	}
	unlock, err := l.lockMember(id)
	if err != nil {
		return err
	}
	defer unlock()
	return l.cleanup(id)
}

// cleanup is Cleanup for a caller that holds the member lock.
func (l *Launcher) cleanup(id string) error {
	l.Sys.Run(ocispec.Runtimes["runc"], []string{"--root", l.runcRoot(), "delete", "--force", id}, nil, nil)
	if m, err := l.readSpec(id); err == nil {
		for i := len(m.Networks) - 1; i >= 0; i-- {
			l.cni("DEL", id, "bridge", m.Networks[i].Interface, cniConfig(m.Networks[i], i == 0))
		}
		l.cni("DEL", id, "loopback", "lo", loopbackConfig)
	}
	l.Sys.DeleteNetNS(l.netnsPath(id))
	return l.Sys.Unmount(filepath.Join(l.memberDir(id), "rootfs"))
}

func (l *Launcher) lockMember(id string) (func(), error) {
	dir := filepath.Join(l.State, "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, id), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if l.lockWaiting != nil {
		l.lockWaiting(id)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// Remove stops a member if it runs, then deletes its writable layer and the given volumes.
func (l *Launcher) Remove(r protocol.Remove) error {
	if err := l.Cleanup(r.ID); err != nil {
		return err
	}
	if err := os.RemoveAll(l.memberDir(r.ID)); err != nil {
		return err
	}
	for _, v := range r.Volumes {
		if err := checkName("volume key", v); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(l.Data, "volumes", v)); err != nil {
			return err
		}
	}
	return nil
}

// Boot deletes the guest state that the project no longer has: members, volumes and layers.
func (l *Launcher) Boot(b protocol.Boot) error {
	if b.MembersMemoryMax > 0 {
		if err := l.Sys.SetMembersMemoryMax(b.MembersMemoryMax); err != nil {
			return fmt.Errorf("limit the members cgroup: %w", err)
		}
	}
	keep := map[string]bool{}
	for _, id := range b.Keep {
		keep[id] = true
	}
	usedLayers := map[string]bool{}
	members, _ := os.ReadDir(filepath.Join(l.Data, "members"))
	for _, e := range members {
		id := e.Name()
		if !keep[id] {
			if checkName("member ID", id) == nil {
				l.Cleanup(id)
			}
			if err := os.RemoveAll(l.memberDir(id)); err != nil {
				return err
			}
			continue
		}
		if m, err := l.readSpec(id); err == nil {
			for _, c := range rootfs.ChainIDs(m.DiffIDs) {
				usedLayers[filepath.Base(l.layerDir(c))] = true
			}
		}
	}
	volumes := map[string]bool{}
	for _, v := range b.Volumes {
		volumes[v] = true
	}
	for _, sub := range []struct {
		dir  string
		keep map[string]bool
	}{{"volumes", volumes}, {"layers", usedLayers}} {
		entries, _ := os.ReadDir(filepath.Join(l.Data, sub.dir))
		for _, e := range entries {
			if e.Name() == ".lock" || sub.keep[e.Name()] {
				continue
			}
			if err := os.RemoveAll(filepath.Join(l.Data, sub.dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteHosts rewrites /etc/hosts of members in place: the bind mount in a running member keeps
// the inode, so it sees the new content.
func (l *Launcher) WriteHosts(content map[string]string) error {
	for id, text := range content {
		if checkName("member ID", id) != nil {
			continue
		}
		f, err := os.OpenFile(filepath.Join(l.memberDir(id), "hosts"), os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		_, err = f.WriteString(text)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}
