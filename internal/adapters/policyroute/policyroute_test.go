package policyroute

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/h44z/wg-portal/internal/domain"
)

func TestIsOurRule(t *testing.T) {
	rule := func(pref, table int, iif string, mark uint32, src *net.IPNet) netlink.Rule {
		r := netlink.NewRule()
		r.Priority, r.Table, r.IifName, r.Mark, r.Src = pref, table, iif, mark, src
		return *r
	}
	src := &net.IPNet{IP: net.IPv4(10, 0, 0, 2), Mask: net.CIDRMask(32, 32)}
	cases := []struct {
		name string
		rule netlink.Rule
		want bool
	}{
		{"bypass", rule(domain.RulePrefBypass, unix.RT_TABLE_MAIN, "wg0", domain.PolicyBypassMark, nil), true},
		{"probe", rule(domain.RulePrefProbe, unix.RT_TABLE_MAIN, "wg0", 0, src), true},
		{"exit", rule(domain.RulePrefExit, domain.ExitRoutingTable(7), "wg0", 0, nil), true},
		{"foreign table at our pref", rule(domain.RulePrefProbe, 100, "", 0, src), false},
		{"foreign rule without iif", rule(domain.RulePrefBypass, unix.RT_TABLE_MAIN, "", 0, nil), false},
		{"foreign exit pref", rule(domain.RulePrefExit, 200, "eth0", 0, nil), false},
		{"other pref", rule(5300, unix.RT_TABLE_MAIN, "wg0", domain.PolicyBypassMark, nil), false},
	}
	for _, c := range cases {
		if got := isOurRule(c.rule); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
