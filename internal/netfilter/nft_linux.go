package netfilter

import (
	"fmt"
	"net"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// TableName is the nftables table (family ip) that the launcher owns.
const TableName = "cs-project"

var table = &nftables.Table{Family: nftables.TableFamilyIPv4, Name: TableName}

var accept = nftables.ChainPolicyAccept

func chains() map[Chain]*nftables.Chain {
	return map[Chain]*nftables.Chain{
		Prerouting: {Name: string(Prerouting), Table: table, Type: nftables.ChainTypeNAT,
			Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityNATDest},
		Forward: {Name: string(Forward), Table: table, Type: nftables.ChainTypeFilter,
			Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter, Policy: &accept},
		Postrouting: {Name: string(Postrouting), Table: table, Type: nftables.ChainTypeNAT,
			Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource},
	}
}

// Apply replaces the table with the rules of in in one batch. A second call with the same
// input leaves the same table.
func Apply(in Input) error {
	rules, err := Rules(in)
	if err != nil {
		return err
	}
	c, err := nftables.New()
	if err != nil {
		return err
	}
	return apply(c, rules)
}

func apply(c *nftables.Conn, rules []Rule) error {
	c.AddTable(table)
	cs := chains()
	for _, name := range []Chain{Prerouting, Forward, Postrouting} {
		c.AddChain(cs[name])
	}
	c.FlushTable(table)
	for _, r := range rules {
		c.AddRule(&nftables.Rule{Table: table, Chain: cs[r.Chain], Exprs: exprs(r)})
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("nftables table %s: %w", TableName, err)
	}
	return nil
}

// Remove deletes the table. It does nothing when the table is absent.
func Remove() error {
	c, err := nftables.New()
	if err != nil {
		return err
	}
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return err
	}
	for _, t := range tables {
		if t.Name == TableName {
			c.DelTable(table)
			return c.Flush()
		}
	}
	return nil
}

func ifname(n string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, n)
	return b
}

func match(reg uint32, data []byte) *expr.Cmp {
	return &expr.Cmp{Op: expr.CmpOpEq, Register: reg, Data: data}
}

func exprs(r Rule) []expr.Any {
	var out []expr.Any
	if r.IIF != "" {
		out = append(out, &expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1}, match(1, ifname(r.IIF)))
	}
	if r.OIF != "" {
		out = append(out, &expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1}, match(1, ifname(r.OIF)))
	}
	if r.Src.IsValid() {
		addr := r.Src.Addr().As4()
		out = append(out,
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
				Mask: net.CIDRMask(r.Src.Bits(), 32), Xor: make([]byte, 4)},
			match(1, addr[:]))
	}
	if r.Proto != "" {
		proto := byte(unix.IPPROTO_TCP)
		if r.Proto == UDP {
			proto = unix.IPPROTO_UDP
		}
		out = append(out, &expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1}, match(1, []byte{proto}))
	}
	if r.DPort != 0 {
		out = append(out,
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			match(1, binaryutil.BigEndian.PutUint16(r.DPort)))
	}
	switch r.Action {
	case Drop:
		out = append(out, &expr.Verdict{Kind: expr.VerdictDrop})
	case Masquerade:
		out = append(out, &expr.Masq{})
	case DNAT:
		addr := r.To.Addr().As4()
		out = append(out,
			&expr.Immediate{Register: 1, Data: addr[:]},
			&expr.Immediate{Register: 2, Data: binaryutil.BigEndian.PutUint16(r.To.Port())},
			&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1, RegProtoMin: 2, Specified: true})
	}
	return out
}
