// Package netcni assigns the bridges, subnets and addresses of the project networks in the VM.
// Network n is the bridge csbr<n> with the subnet 10.213.<n>.0/24 and the gateway 10.213.<n>.1.
package netcni

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/beshkenadze/cs-project/internal/protocol"
)

const (
	bridgePrefix = "csbr"
	maxNetworks  = 256
	firstHost    = 2
	lastHost     = 254
)

// Bridge gives the bridge name of network n.
func Bridge(n int) string { return bridgePrefix + strconv.Itoa(n) }

// Subnet gives the subnet of network n.
func Subnet(n int) netip.Prefix {
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 213, byte(n), 0}), 24)
}

// Gateway gives the gateway address of network n, the address of its bridge.
func Gateway(n int) netip.Addr { return netip.AddrFrom4([4]byte{10, 213, byte(n), 1}) }

func bridgeIndex(bridge string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(bridge, bridgePrefix))
	if err != nil || n < 0 || n >= maxNetworks || Bridge(n) != bridge {
		return 0, fmt.Errorf("bridge name %q", bridge)
	}
	return n, nil
}

// Request is one network of a member.
type Request struct {
	Name     string
	Internal bool
}

// Allocator gives each network one subnet and each endpoint one address. It holds the networks
// of each member; a network keeps its subnet while a member uses it.
type Allocator struct {
	members map[string][]protocol.Network
}

// state is what the members use now.
type state struct {
	index map[string]int // network name to network number
	name  map[int]string // network number to network name
	used  map[netip.Addr]string
}

// NewAllocator starts from the networks that existing members already have, by member ID.
func NewAllocator(existing map[string][]protocol.Network) (*Allocator, error) {
	a := &Allocator{members: map[string][]protocol.Network{}}
	for id, nets := range existing {
		a.members[id] = slices.Clone(nets)
	}
	if _, err := a.state(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Allocator) state() (*state, error) {
	s := &state{index: map[string]int{}, name: map[int]string{}, used: map[netip.Addr]string{}}
	for id, nets := range a.members {
		for _, n := range nets {
			i, err := bridgeIndex(n.Bridge)
			if err != nil {
				return nil, fmt.Errorf("member %s: %w", id, err)
			}
			p, err := netip.ParsePrefix(n.Address)
			if err != nil {
				return nil, fmt.Errorf("member %s: %w", id, err)
			}
			if p.Bits() != 24 || !Subnet(i).Contains(p.Addr()) || p.Addr().As4()[3] < firstHost || p.Addr().As4()[3] > lastHost {
				return nil, fmt.Errorf("member %s: address %s is not a host of %s", id, n.Address, n.Bridge)
			}
			if j, ok := s.index[n.Name]; ok && j != i {
				return nil, fmt.Errorf("network %s is on %s and %s", n.Name, Bridge(j), n.Bridge)
			}
			if other, ok := s.name[i]; ok && other != n.Name {
				return nil, fmt.Errorf("networks %s and %s are on %s", other, n.Name, n.Bridge)
			}
			if other, ok := s.used[p.Addr()]; ok {
				return nil, fmt.Errorf("address %s is used by %s and %s", p.Addr(), other, id)
			}
			s.index[n.Name], s.name[i], s.used[p.Addr()] = i, n.Name, id
		}
	}
	return s, nil
}

// Attach gives member one interface per requested network: eth0, eth1, and so on. Networks that
// are not internal come first, so that eth0, which gets the default route, has egress.
func (a *Allocator) Attach(member string, reqs []Request) ([]protocol.Network, error) {
	if _, ok := a.members[member]; ok {
		return nil, fmt.Errorf("member %s already has networks", member)
	}
	s, err := a.state()
	if err != nil {
		return nil, err
	}
	ordered := slices.Clone(reqs)
	slices.SortStableFunc(ordered, func(x, y Request) int {
		switch {
		case x.Internal == y.Internal:
			return 0
		case y.Internal:
			return -1
		}
		return 1
	})
	out := make([]protocol.Network, 0, len(ordered))
	for i, r := range ordered {
		if r.Name == "" {
			return nil, fmt.Errorf("member %s: network with no name", member)
		}
		if slices.ContainsFunc(out, func(n protocol.Network) bool { return n.Name == r.Name }) {
			return nil, fmt.Errorf("member %s: network %s is requested twice", member, r.Name)
		}
		n, ok := s.index[r.Name]
		if !ok {
			if n, ok = freeNetwork(s); !ok {
				return nil, fmt.Errorf("no free subnet for network %s", r.Name)
			}
			s.index[r.Name], s.name[n] = n, r.Name
		}
		addr, ok := freeHost(s, n)
		if !ok {
			return nil, fmt.Errorf("network %s has no free address", r.Name)
		}
		s.used[addr] = member
		out = append(out, protocol.Network{
			Name: r.Name, Bridge: Bridge(n), Address: netip.PrefixFrom(addr, 24).String(),
			Gateway: Gateway(n).String(), Interface: "eth" + strconv.Itoa(i),
		})
	}
	a.members[member] = out
	return slices.Clone(out), nil
}

// Release frees the addresses of member, and each network that no other member uses.
func (a *Allocator) Release(member string) {
	delete(a.members, member)
}

func freeNetwork(s *state) (int, bool) {
	for n := range maxNetworks {
		if _, ok := s.name[n]; !ok {
			return n, true
		}
	}
	return 0, false
}

func freeHost(s *state, n int) (netip.Addr, bool) {
	for h := firstHost; h <= lastHost; h++ {
		a := netip.AddrFrom4([4]byte{10, 213, byte(n), byte(h)})
		if _, ok := s.used[a]; !ok {
			return a, true
		}
	}
	return netip.Addr{}, false
}
