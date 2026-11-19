// Package netfilter builds and applies the nftables table of the project VM: egress through the
// uplink, isolation between bridges, internal networks and published ports.
package netfilter

import (
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"
)

// Uplink is the interface of the VM that Apple connects to the host.
const Uplink = "eth0"

// ForwardPath is the sysctl that enables IPv4 forwarding.
const ForwardPath = "/proc/sys/net/ipv4/ip_forward"

// Proto is a transport protocol of a published port.
type Proto string

const (
	TCP Proto = "tcp"
	UDP Proto = "udp"
)

// Network is one bridge in the VM.
type Network struct {
	Bridge   string
	Subnet   netip.Prefix
	Internal bool // no egress and no published port
}

// Port is one published port. Addresses are the addresses of the member, one per network.
type Port struct {
	Proto     Proto
	HostPort  uint16
	Port      uint16
	Addresses []netip.Addr
}

// Input is the state that the table enforces.
type Input struct {
	Networks []Network
	Ports    []Port
}

type Chain string

const (
	Prerouting  Chain = "prerouting"
	Forward     Chain = "forward"
	Postrouting Chain = "postrouting"
)

type Action string

const (
	DNAT       Action = "dnat"
	Drop       Action = "drop"
	Masquerade Action = "masquerade"
)

// Rule is one rule of the table. An empty field matches each packet.
type Rule struct {
	Chain  Chain
	IIF    string
	OIF    string
	Src    netip.Prefix
	Proto  Proto
	DPort  uint16
	Action Action
	To     netip.AddrPort
}

// Rules gives the rules of in: prerouting, then forward, then postrouting, each in a fixed order.
func Rules(in Input) ([]Rule, error) {
	nets, err := checkNetworks(in.Networks)
	if err != nil {
		return nil, err
	}
	dnat, err := dnatRules(nets, in.Ports)
	if err != nil {
		return nil, err
	}
	out := dnat
	for _, n := range nets {
		if n.Internal {
			out = append(out,
				Rule{Chain: Forward, IIF: n.Bridge, OIF: Uplink, Action: Drop},
				Rule{Chain: Forward, IIF: Uplink, OIF: n.Bridge, Action: Drop})
		}
	}
	for _, a := range nets {
		for _, b := range nets {
			if a.Bridge != b.Bridge {
				out = append(out, Rule{Chain: Forward, IIF: a.Bridge, OIF: b.Bridge, Action: Drop})
			}
		}
	}
	for _, n := range nets {
		if !n.Internal {
			out = append(out, Rule{Chain: Postrouting, OIF: Uplink, Src: n.Subnet, Action: Masquerade})
		}
	}
	return out, nil
}

// checkNetworks returns the networks sorted by subnet.
func checkNetworks(in []Network) ([]Network, error) {
	nets := slices.Clone(in)
	slices.SortFunc(nets, func(a, b Network) int { return a.Subnet.Addr().Compare(b.Subnet.Addr()) })
	for i, n := range nets {
		if n.Bridge == "" || len(n.Bridge) > 15 || n.Bridge == Uplink {
			return nil, fmt.Errorf("bridge name %q", n.Bridge)
		}
		if !n.Subnet.Addr().Is4() || n.Subnet != n.Subnet.Masked() {
			return nil, fmt.Errorf("bridge %s: subnet %s is not an IPv4 network address", n.Bridge, n.Subnet)
		}
		for _, m := range nets[:i] {
			if m.Bridge == n.Bridge {
				return nil, fmt.Errorf("bridge %s is listed twice", n.Bridge)
			}
			if m.Subnet.Overlaps(n.Subnet) {
				return nil, fmt.Errorf("subnets %s and %s overlap", m.Subnet, n.Subnet)
			}
		}
	}
	return nets, nil
}

// dnatRules gives one rule per port, to the first member address on a network that is not
// internal. A port of a member only on internal networks gets no rule.
func dnatRules(nets []Network, ports []Port) ([]Rule, error) {
	var out []Rule
	for _, p := range ports {
		if p.Proto != TCP && p.Proto != UDP {
			return nil, fmt.Errorf("port %d: protocol %q", p.HostPort, p.Proto)
		}
		if p.HostPort == 0 || p.Port == 0 {
			return nil, fmt.Errorf("port %d/%s: zero port", p.HostPort, p.Proto)
		}
		var to netip.Addr
		for _, a := range p.Addresses {
			n, ok := networkOf(nets, a)
			if !ok {
				return nil, fmt.Errorf("port %d/%s: address %s is on no network", p.HostPort, p.Proto, a)
			}
			if !n.Internal && !to.IsValid() {
				to = a
			}
		}
		for _, r := range out {
			if r.Proto == p.Proto && r.DPort == p.HostPort {
				return nil, fmt.Errorf("port %d/%s is published twice", p.HostPort, p.Proto)
			}
		}
		rule := Rule{Chain: Prerouting, IIF: Uplink, Proto: p.Proto, DPort: p.HostPort, Action: DNAT}
		if to.IsValid() {
			rule.To = netip.AddrPortFrom(to, p.Port)
		}
		out = append(out, rule)
	}
	// Ports with no target were kept until here so that duplicates are found.
	out = slices.DeleteFunc(out, func(r Rule) bool { return !r.To.IsValid() })
	slices.SortFunc(out, func(a, b Rule) int {
		if c := strings.Compare(string(a.Proto), string(b.Proto)); c != 0 {
			return c
		}
		return int(a.DPort) - int(b.DPort)
	})
	return out, nil
}

func networkOf(nets []Network, a netip.Addr) (Network, bool) {
	for _, n := range nets {
		if n.Subnet.Contains(a) {
			return n, true
		}
	}
	return Network{}, false
}

// EnableForwarding writes 1 to the sysctl file at path, normally ForwardPath.
func EnableForwarding(path string) error {
	return os.WriteFile(path, []byte("1\n"), 0o644)
}
