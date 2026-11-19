package netfilter

import (
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

var (
	front = Network{Bridge: "csbr0", Subnet: netip.MustParsePrefix("10.213.0.0/24")}
	back  = Network{Bridge: "csbr1", Subnet: netip.MustParsePrefix("10.213.1.0/24")}
	priv  = Network{Bridge: "csbr2", Subnet: netip.MustParsePrefix("10.213.2.0/24"), Internal: true}
)

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, a := range s {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

func TestRulesTwoBridgesTwoPorts(t *testing.T) {
	got, err := Rules(Input{
		Networks: []Network{back, front},
		Ports: []Port{
			{Proto: UDP, HostPort: 5353, Port: 53, Addresses: addrs("10.213.1.3")},
			{Proto: TCP, HostPort: 8080, Port: 80, Addresses: addrs("10.213.0.2")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Rule{
		{Chain: Prerouting, IIF: Uplink, Proto: TCP, DPort: 8080, Action: DNAT, To: netip.MustParseAddrPort("10.213.0.2:80")},
		{Chain: Prerouting, IIF: Uplink, Proto: UDP, DPort: 5353, Action: DNAT, To: netip.MustParseAddrPort("10.213.1.3:53")},
		{Chain: Forward, IIF: "csbr0", OIF: "csbr1", Action: Drop},
		{Chain: Forward, IIF: "csbr1", OIF: "csbr0", Action: Drop},
		{Chain: Postrouting, OIF: Uplink, Src: front.Subnet, Action: Masquerade},
		{Chain: Postrouting, OIF: Uplink, Src: back.Subnet, Action: Masquerade},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rules:\n got %+v\nwant %+v", got, want)
	}
}

func TestRulesInternalNetwork(t *testing.T) {
	got, err := Rules(Input{
		Networks: []Network{front, priv},
		Ports: []Port{
			// Only on the internal network: no DNAT.
			{Proto: TCP, HostPort: 8081, Port: 80, Addresses: addrs("10.213.2.5")},
			// On the internal and a normal network: DNAT to the normal address.
			{Proto: TCP, HostPort: 8082, Port: 80, Addresses: addrs("10.213.2.6", "10.213.0.6")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Rule{
		{Chain: Prerouting, IIF: Uplink, Proto: TCP, DPort: 8082, Action: DNAT, To: netip.MustParseAddrPort("10.213.0.6:80")},
		{Chain: Forward, IIF: "csbr2", OIF: Uplink, Action: Drop},
		{Chain: Forward, IIF: Uplink, OIF: "csbr2", Action: Drop},
		{Chain: Forward, IIF: "csbr0", OIF: "csbr2", Action: Drop},
		{Chain: Forward, IIF: "csbr2", OIF: "csbr0", Action: Drop},
		{Chain: Postrouting, OIF: Uplink, Src: front.Subnet, Action: Masquerade},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rules:\n got %+v\nwant %+v", got, want)
	}
}

func TestRulesRefuseBadInput(t *testing.T) {
	for name, in := range map[string]Input{
		"duplicate port": {Networks: []Network{front}, Ports: []Port{
			{Proto: TCP, HostPort: 80, Port: 80, Addresses: addrs("10.213.0.2")},
			{Proto: TCP, HostPort: 80, Port: 81, Addresses: addrs("10.213.0.3")},
		}},
		"address outside the networks": {Networks: []Network{front}, Ports: []Port{
			{Proto: TCP, HostPort: 80, Port: 80, Addresses: addrs("10.213.9.2")},
		}},
		"unknown protocol": {Networks: []Network{front}, Ports: []Port{
			{Proto: "sctp", HostPort: 80, Port: 80, Addresses: addrs("10.213.0.2")},
		}},
		"duplicate bridge":   {Networks: []Network{front, {Bridge: "csbr0", Subnet: back.Subnet}}},
		"unmasked subnet":    {Networks: []Network{{Bridge: "csbr0", Subnet: netip.MustParsePrefix("10.213.0.1/24")}}},
		"long bridge name":   {Networks: []Network{{Bridge: "csbr0123456789012", Subnet: front.Subnet}}},
		"overlapping subnet": {Networks: []Network{front, {Bridge: "csbr1", Subnet: netip.MustParsePrefix("10.213.0.0/16")}}},
	} {
		if _, err := Rules(in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestEnableForwarding(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ip_forward")
	if err := os.WriteFile(p, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnableForwarding(p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "1\n" {
		t.Fatalf("content %q", b)
	}
}
