package netfilter

import (
	"bytes"
	"net/netip"
	"reflect"
	"slices"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

func TestExprsDNAT(t *testing.T) {
	got := exprs(Rule{Chain: Prerouting, IIF: Uplink, Proto: UDP, DPort: 5353, Action: DNAT,
		To: netip.MustParseAddrPort("10.213.1.3:53")})
	want := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte("eth0\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_UDP}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(5353)},
		&expr.Immediate{Register: 1, Data: []byte{10, 213, 1, 3}},
		&expr.Immediate{Register: 2, Data: binaryutil.BigEndian.PutUint16(53)},
		&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1, RegProtoMin: 2, Specified: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exprs:\n got %#v\nwant %#v", got, want)
	}
}

func TestExprsMasquerade(t *testing.T) {
	got := exprs(Rule{Chain: Postrouting, OIF: Uplink, Src: netip.MustParsePrefix("10.213.0.0/24"), Action: Masquerade})
	want := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte("eth0\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: []byte{255, 255, 255, 0}, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{10, 213, 0, 0}},
		&expr.Masq{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exprs:\n got %#v\nwant %#v", got, want)
	}
}

// recorder keeps the messages of each batch without their netlink headers, and the nftables
// message types in order.
func recorder(t *testing.T, batches *[][]byte, types *[]int) *nftables.Conn {
	t.Helper()
	c, err := nftables.New(nftables.WithTestDial(func(req []netlink.Message) ([]netlink.Message, error) {
		if len(req) == 0 {
			return nil, nil // a receive with nothing buffered
		}
		var batch []byte
		for _, m := range req {
			b, err := m.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			batch = append(batch, b[16:]...)
			*types = append(*types, int(m.Header.Type)&0xff)
		}
		*batches = append(*batches, batch)
		return req, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestApplyTwiceSendsTheSameBatch(t *testing.T) {
	rules, err := Rules(Input{Networks: []Network{front, back, priv}, Ports: []Port{
		{Proto: TCP, HostPort: 8080, Port: 80, Addresses: addrs("10.213.0.2")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var batches [][]byte
	var types []int
	c := recorder(t, &batches, &types)
	if err := apply(c, rules); err != nil {
		t.Fatal(err)
	}
	if err := apply(c, rules); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || !bytes.Equal(batches[0], batches[1]) {
		t.Fatalf("batches differ: %d", len(batches))
	}
	if !bytes.Contains(batches[0], []byte(TableName+"\x00")) {
		t.Fatal("batch does not name the table")
	}
	// The flush of the table comes before the first rule, so a second apply replaces the rules.
	flush, add := slices.Index(types, unix.NFT_MSG_DELRULE), slices.Index(types, unix.NFT_MSG_NEWRULE)
	if flush < 0 || add < 0 || flush > add {
		t.Fatalf("message types %v", types)
	}
}
