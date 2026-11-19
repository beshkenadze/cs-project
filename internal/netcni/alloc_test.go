package netcni

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/beshkenadze/cs-project/internal/protocol"
)

func mustAttach(t *testing.T, a *Allocator, member string, reqs ...Request) []protocol.Network {
	t.Helper()
	got, err := a.Attach(member, reqs)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAttachMemberOnTwoNetworks(t *testing.T) {
	a, err := NewAllocator(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := mustAttach(t, a, "m1", Request{Name: "p_front"}, Request{Name: "p_back"})
	want := []protocol.Network{
		{Name: "p_front", Bridge: "csbr0", Address: "10.213.0.2/24", Gateway: "10.213.0.1", Interface: "eth0"},
		{Name: "p_back", Bridge: "csbr1", Address: "10.213.1.2/24", Gateway: "10.213.1.1", Interface: "eth1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("m1:\n got %+v\nwant %+v", got, want)
	}
	got = mustAttach(t, a, "m2", Request{Name: "p_back"})
	want = []protocol.Network{
		{Name: "p_back", Bridge: "csbr1", Address: "10.213.1.3/24", Gateway: "10.213.1.1", Interface: "eth0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("m2:\n got %+v\nwant %+v", got, want)
	}
}

func TestAttachPutsANormalNetworkFirst(t *testing.T) {
	a, _ := NewAllocator(nil)
	got := mustAttach(t, a, "m1", Request{Name: "p_priv", Internal: true}, Request{Name: "p_front"})
	// The launcher gives the default route to eth0, so egress leaves through the normal network.
	if got[0].Name != "p_front" || got[0].Interface != "eth0" || got[1].Name != "p_priv" || got[1].Interface != "eth1" {
		t.Fatalf("order %+v", got)
	}
}

func TestNewAllocatorKeepsExistingAddresses(t *testing.T) {
	a, err := NewAllocator(map[string][]protocol.Network{
		"old": {{Name: "p_default", Bridge: "csbr0", Address: "10.213.0.2/24", Gateway: "10.213.0.1", Interface: "eth0"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := mustAttach(t, a, "new", Request{Name: "p_other"}, Request{Name: "p_default"})
	if got[0].Bridge != "csbr1" || got[1].Bridge != "csbr0" || got[1].Address != "10.213.0.3/24" {
		t.Fatalf("got %+v", got)
	}
}

func TestReleaseFreesAddressesAndNetworks(t *testing.T) {
	a, _ := NewAllocator(nil)
	mustAttach(t, a, "m1", Request{Name: "p_a"})
	a.Release("m1")
	got := mustAttach(t, a, "m2", Request{Name: "p_b"})
	if got[0].Bridge != "csbr0" || got[0].Address != "10.213.0.2/24" {
		t.Fatalf("got %+v", got)
	}
}

func TestAttachErrors(t *testing.T) {
	a, _ := NewAllocator(nil)
	mustAttach(t, a, "m1", Request{Name: "p_a"})
	if _, err := a.Attach("m1", []Request{{Name: "p_b"}}); err == nil {
		t.Error("second attach of a member: no error")
	}
	if _, err := a.Attach("m2", []Request{{Name: "p_a"}, {Name: "p_a"}}); err == nil {
		t.Error("network twice: no error")
	}
	if _, err := a.Attach("m2", []Request{{Name: ""}}); err == nil {
		t.Error("empty name: no error")
	}
	for i := 3; i <= 254; i++ {
		mustAttach(t, a, fmt.Sprint("fill", i), Request{Name: "p_a"})
	}
	if _, err := a.Attach("m3", []Request{{Name: "p_a"}}); err == nil {
		t.Error("full subnet: no error")
	}
	if _, ok := a.members["m3"]; ok {
		t.Error("a failed attach kept the member")
	}
}

func TestNewAllocatorRefusesConflicts(t *testing.T) {
	for name, existing := range map[string]map[string][]protocol.Network{
		"same address": {
			"a": {{Name: "n", Bridge: "csbr0", Address: "10.213.0.2/24"}},
			"b": {{Name: "n", Bridge: "csbr0", Address: "10.213.0.2/24"}},
		},
		"one network on two bridges": {
			"a": {{Name: "n", Bridge: "csbr0", Address: "10.213.0.2/24"}},
			"b": {{Name: "n", Bridge: "csbr1", Address: "10.213.1.2/24"}},
		},
		"two networks on one bridge": {
			"a": {{Name: "n", Bridge: "csbr0", Address: "10.213.0.2/24"}},
			"b": {{Name: "m", Bridge: "csbr0", Address: "10.213.0.3/24"}},
		},
		"address outside the bridge subnet": {
			"a": {{Name: "n", Bridge: "csbr0", Address: "10.213.1.2/24"}},
		},
		"gateway address": {
			"a": {{Name: "n", Bridge: "csbr0", Address: "10.213.0.1/24"}},
		},
		"bad bridge name": {
			"a": {{Name: "n", Bridge: "br0", Address: "10.213.0.2/24"}},
		},
	} {
		if _, err := NewAllocator(existing); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
