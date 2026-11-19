// Package ocispec builds the runc config.json of a member from its spec.
package ocispec

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/beshkenadze/cs-project/internal/caps"
	"github.com/beshkenadze/cs-project/internal/cgroups"
	"github.com/beshkenadze/cs-project/internal/protocol"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Runtimes maps the ociRuntime of a member to its binary. This is the only place that chooses an
// OCI runtime.
var Runtimes = map[string]string{"runc": "/bin/runc"}

// Paths are the guest paths that the launcher prepared for a member.
type Paths struct {
	Rootfs   string
	Hosts    string
	Resolv   string
	Hostname string
	NetNS    string
	Volumes  string // directory of the anonymous volumes
}

const defaultPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Docker defaults.
var (
	maskedPaths = []string{
		"/proc/asound", "/proc/acpi", "/proc/interrupts", "/proc/kcore", "/proc/keys", "/proc/latency_stats",
		"/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug", "/proc/scsi", "/sys/firmware",
		"/sys/devices/virtual/powercap",
	}
	readonlyPaths = []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"}
)

// Build gives the runtime spec. It refuses a capability outside the member list, an unknown
// runtime and a mount kind that this release does not support.
func Build(m protocol.Member, p Paths) (*specs.Spec, error) {
	if _, ok := Runtimes[m.OCIRuntime]; !ok {
		return nil, fmt.Errorf("unknown OCI runtime %q", m.OCIRuntime)
	}
	capList, err := caps.Effective(m.CapAdd, m.CapDrop)
	if err != nil {
		return nil, err
	}
	uid, gid, groups, err := ResolveUser(p.Rootfs, m.User)
	if err != nil {
		return nil, err
	}
	env := append([]string{}, m.Env...)
	if !hasKey(env, "PATH") {
		env = append([]string{defaultPath}, env...)
	}
	if !hasKey(env, "HOSTNAME") && m.Hostname != "" {
		env = append(env, "HOSTNAME="+m.Hostname)
	}
	cwd := m.Cwd
	if cwd == "" {
		cwd = "/"
	}
	mounts := []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup", Options: []string{"nosuid", "noexec", "nodev", "relatime", "ro"}},
	}
	for _, f := range []struct{ dst, src string }{{"/etc/hosts", p.Hosts}, {"/etc/resolv.conf", p.Resolv}, {"/etc/hostname", p.Hostname}} {
		if f.src != "" {
			mounts = append(mounts, specs.Mount{Destination: f.dst, Type: "bind", Source: f.src, Options: []string{"rbind", "rprivate"}})
		}
	}
	for _, mt := range m.Mounts {
		if mt.Kind != "volume" {
			return nil, fmt.Errorf("mount kind %q is not supported", mt.Kind)
		}
		if mt.Key == "" || strings.ContainsAny(mt.Key, "/.") {
			return nil, fmt.Errorf("invalid volume key %q", mt.Key)
		}
		opts := []string{"rbind", "rprivate"}
		if mt.ReadOnly {
			opts = append(opts, "ro")
		}
		mounts = append(mounts, specs.Mount{Destination: mt.Target, Type: "bind", Source: filepath.Join(p.Volumes, mt.Key), Options: opts})
	}
	limit := m.MemoryMax
	resources := &specs.LinuxResources{Devices: []specs.LinuxDeviceCgroup{{Allow: false, Access: "rwm"}}}
	if limit > 0 {
		resources.Memory = &specs.LinuxMemory{Limit: &limit}
	}
	return &specs.Spec{
		Version: specs.Version,
		Process: &specs.Process{
			Args: m.Args,
			Env:  env,
			Cwd:  cwd,
			User: specs.User{UID: uid, GID: gid, AdditionalGids: groups},
			Capabilities: &specs.LinuxCapabilities{
				Bounding: capList, Effective: capList, Permitted: capList,
			},
			Rlimits: []specs.POSIXRlimit{{Type: "RLIMIT_NOFILE", Hard: 1048576, Soft: 1048576}},
		},
		Root:     &specs.Root{Path: p.Rootfs},
		Hostname: m.Hostname,
		Mounts:   mounts,
		Linux: &specs.Linux{
			CgroupsPath: cgroups.MemberPath(m.ID),
			Resources:   resources,
			Namespaces: []specs.LinuxNamespace{
				{Type: "pid"}, {Type: "ipc"}, {Type: "uts"}, {Type: "mount"}, {Type: "cgroup"},
				{Type: "network", Path: p.NetNS},
			},
			MaskedPaths:   maskedPaths,
			ReadonlyPaths: readonlyPaths,
		},
	}, nil
}

func hasKey(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

type entry struct {
	name    string
	id      uint32
	gid     uint32
	members []string
}

func readDB(path string, group bool) []entry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []entry
	s := bufio.NewScanner(f)
	for s.Scan() {
		parts := strings.Split(s.Text(), ":")
		if len(parts) < 4 || strings.HasPrefix(parts[0], "#") {
			continue
		}
		id, err := strconv.ParseUint(parts[2], 10, 32)
		if err != nil {
			continue
		}
		e := entry{name: parts[0], id: uint32(id)}
		if group {
			if parts[3] != "" {
				e.members = strings.Split(parts[3], ",")
			}
		} else if gid, err := strconv.ParseUint(parts[3], 10, 32); err == nil {
			e.gid = uint32(gid)
		}
		out = append(out, e)
	}
	return out
}

// ResolveUser resolves the Docker user string ("name", "uid", "name:group", "uid:gid") against
// /etc/passwd and /etc/group of the rootfs. The host cannot read the rootfs, so this runs here.
func ResolveUser(rootfs, user string) (uid, gid uint32, groups []uint32, err error) {
	if user == "" {
		return 0, 0, nil, nil
	}
	userPart, groupPart, hasGroup := strings.Cut(user, ":")
	passwd := readDB(filepath.Join(rootfs, "etc/passwd"), false)
	groupDB := readDB(filepath.Join(rootfs, "etc/group"), true)
	name := ""
	if n, perr := strconv.ParseUint(userPart, 10, 32); perr == nil {
		uid = uint32(n)
		for _, e := range passwd {
			if e.id == uid {
				gid, name = e.gid, e.name
				break
			}
		}
	} else {
		found := false
		for _, e := range passwd {
			if e.name == userPart {
				uid, gid, name, found = e.id, e.gid, e.name, true
				break
			}
		}
		if !found {
			return 0, 0, nil, fmt.Errorf("unable to find user %s: no matching entries in passwd file", userPart)
		}
	}
	if hasGroup {
		if n, perr := strconv.ParseUint(groupPart, 10, 32); perr == nil {
			gid = uint32(n)
		} else {
			found := false
			for _, e := range groupDB {
				if e.name == groupPart {
					gid, found = e.id, true
					break
				}
			}
			if !found {
				return 0, 0, nil, errors.New("unable to find group " + groupPart)
			}
		}
	}
	if name != "" {
		for _, e := range groupDB {
			for _, mbr := range e.members {
				if mbr == name && e.id != gid {
					groups = append(groups, e.id)
				}
			}
		}
	}
	return uid, gid, groups, nil
}
