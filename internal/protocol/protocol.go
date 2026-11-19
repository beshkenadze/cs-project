// Package protocol has the JSON types that socktainer and the launcher exchange. Each command
// is one exec of /cs-project with JSON on stdin and stdout.
package protocol

// Version is the protocol version. socktainer refuses a launcher with another value.
const Version = 1

// Paths in the VM container.
const (
	StateDir    = "/run/cs-project/state" // tmpfs: control socket, runc state, network namespaces
	HostDir     = "/run/cs-project/host"  // virtiofs share of the project directory on the host
	DataDir     = "/data"                 // the data volume
	ControlSock = StateDir + "/ctl.sock"
)

// Mount is one mount of a member. Kind "volume" with Key is the anonymous volume
// /data/volumes/<Key>.
type Mount struct {
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// Network is one interface of a member. socktainer assigns the address.
type Network struct {
	Name      string `json:"name"`
	Bridge    string `json:"bridge"`
	Address   string `json:"address"` // CIDR, for example 10.213.0.11/24
	Gateway   string `json:"gateway"`
	Interface string `json:"interface"` // eth0, eth1, ...
}

// Member is the spec of one member.
type Member struct {
	ID         string    `json:"id"`
	Hostname   string    `json:"hostname"`
	Args       []string  `json:"args"`
	Env        []string  `json:"env"`
	Cwd        string    `json:"cwd"`
	User       string    `json:"user"`
	DiffIDs    []string  `json:"diffIDs"` // uncompressed layer digests, base first
	Blobs      []string  `json:"blobs"`   // blob digests in HostDir/blobs, same order
	Mounts     []Mount   `json:"mounts"`
	Networks   []Network `json:"networks"`
	MemoryMax  int64     `json:"memoryMax"`
	Hosts      string    `json:"hosts"` // first content of /etc/hosts; `hosts` rewrites it
	CapAdd     []string  `json:"capAdd,omitempty"`
	CapDrop    []string  `json:"capDrop,omitempty"`
	OCIRuntime string    `json:"ociRuntime"`
}

// Boot is the input of `boot`: the members that the project still has. Guest state of other
// IDs is deleted, and so are layers and volumes that none of them uses.
type Boot struct {
	Keep    []string `json:"keep"`
	Volumes []string `json:"volumes"`
	// MembersMemoryMax is memory.max of the members/ cgroup: the VM memory minus the reserve
	// of the launcher. Zero leaves it unset.
	MembersMemoryMax uint64 `json:"membersMemoryMax"`
}

// Remove is the input of `remove`.
type Remove struct {
	ID      string   `json:"id"`
	Volumes []string `json:"volumes"` // anonymous volumes with no reference left
}

// MemberState is one member in a snapshot.
type MemberState struct {
	ID         string `json:"id"`
	Running    bool   `json:"running"`
	PID        int    `json:"pid,omitempty"`
	ExitCode   int    `json:"exitCode"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

// Event is one line of `events`. The first line has Type "snapshot" and Members.
type Event struct {
	Seq     uint64        `json:"seq"`
	Type    string        `json:"type"` // snapshot, start, exit
	ID      string        `json:"id,omitempty"`
	PID     int           `json:"pid,omitempty"`
	Code    int           `json:"code,omitempty"`
	Time    string        `json:"time,omitempty"`
	Members []MemberState `json:"members,omitempty"`
}

// Request is one line on the control socket.
type Request struct {
	Op  string `json:"op"` // started, subscribe
	ID  string `json:"id,omitempty"`
	PID int    `json:"pid,omitempty"`
}
