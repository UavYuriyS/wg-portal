// Package policyroute applies the routing policy (exit node, bypass and blackhole lists) to the Linux kernel:
// ipsets via netlink, iptables chains via the iptables/ip6tables binaries, ip rules and exit tables via netlink.
//
// Debugging on a node: ip rule; ip route show table <0x57000000 + link index>; ipset list -t;
// iptables -S WGP-FWD; iptables -t mangle -S WGP-MARK (and the ip6tables equivalents).
package policyroute

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/h44z/wg-portal/internal/config"
	"github.com/h44z/wg-portal/internal/domain"
)

const (
	chainForward = "WGP-FWD"  // filter table, jumped from FORWARD position 1
	chainMark    = "WGP-MARK" // mangle table, jumped from PREROUTING position 1
	setMaxElem   = 1 << 20
	dnsTimeout   = 3600 // default timeout of DNS sets, entries always carry their own
)

var markMask = fmt.Sprintf("0x%x/0x%x", domain.PolicyBypassMark, domain.PolicyBypassMark)

type family struct {
	nl       int    // netlink family
	ipt      string // iptables binary
	suffix   string // set name suffix
	icmpDeny string
	bits     int
}

var families = []family{
	{netlink.FAMILY_V4, "iptables", "4", "icmp-admin-prohibited", 32},
	{netlink.FAMILY_V6, "ip6tables", "6", "icmp6-adm-prohibited", 128},
}

// set names: wgp-block4, wgp-block-dns6, wgp-bypass4, ...
func setName(kind domain.RoutingListKind, dns bool, f family) string {
	name := "wgp-bypass"
	if kind == domain.RoutingListBlackhole {
		name = "wgp-block"
	}
	if dns {
		name += "-dns"
	}
	return name + f.suffix
}

type Controller struct {
	cfg *config.Config

	mu         sync.Mutex
	staticHash [32]byte // static set content applied by this process
}

func NewController(cfg *config.Config) *Controller {
	return &Controller{cfg: cfg}
}

// region apply

func (c *Controller) Apply(ctx context.Context, spec domain.PolicySpec) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !spec.Enabled() {
		c.cleanup(ctx)
		return nil
	}
	links := map[string]netlink.Link{}
	for _, pi := range spec.Interfaces {
		link, err := netlink.LinkByName(pi.Name)
		if err != nil {
			slog.Debug("policyroute: interface not present yet, skipping its exit table", "interface", pi.Name)
			continue
		}
		table := domain.ExitRoutingTable(link.Attrs().Index)
		if table == pi.Table || table == c.cfg.Advanced.RouteTableOffset+link.Attrs().Index {
			return fmt.Errorf("exit table %d of %s collides with its routing table, change "+
				"advanced.route_table_offset or the interface routing table", table, pi.Name)
		}
		links[pi.Name] = link
	}

	if err := c.applySets(spec); err != nil {
		return err
	}
	for _, f := range families {
		err := c.applyChains(ctx, f, spec)
		if err != nil && f.nl == netlink.FAMILY_V6 {
			slog.Warn("policyroute: IPv6 firewall rules failed, IPv6 is not filtered", "error", err)
			continue
		}
		if err != nil {
			return err
		}
	}
	if err := c.applyRules(spec, links); err != nil {
		return err
	}
	return c.applyExitRoutes(spec, links)
}

func (c *Controller) applySets(spec domain.PolicySpec) error {
	h := sha256.New()
	_, _ = fmt.Fprint(h, spec.Block, spec.Bypass)
	var hash [32]byte
	copy(hash[:], h.Sum(nil))

	dnsTo := uint32(dnsTimeout)
	for _, f := range families {
		for _, kind := range []domain.RoutingListKind{domain.RoutingListBlackhole, domain.RoutingListBypass} {
			name := setName(kind, true, f)
			if err := createSet(name, f, &dnsTo, false); err != nil {
				return err
			}
			if slices.Contains(spec.FlushDns, kind) {
				if err := netlink.IpsetFlush(name); err != nil {
					return fmt.Errorf("ipset flush %s: %w", name, err)
				}
				slog.Debug("policyroute: flushed dns set", "set", name)
			}
			static := spec.Bypass
			if kind == domain.RoutingListBlackhole {
				static = spec.Block
			}
			if hash != c.staticHash || !setExists(setName(kind, false, f)) {
				if err := rebuildSet(setName(kind, false, f), f, static); err != nil {
					return err
				}
			}
		}
	}
	c.staticHash = hash
	return nil
}

func setExists(name string) bool {
	_, err := netlink.IpsetList(name)
	return err == nil
}

func createSet(name string, f family, timeout *uint32, exclusive bool) error {
	err := netlink.IpsetCreate(name, "hash:net", netlink.IpsetCreateOptions{
		Replace: !exclusive, Family: uint8(f.nl), MaxElements: setMaxElem, Timeout: timeout,
	})
	if err != nil {
		return fmt.Errorf("ipset create %s: %w", name, err)
	}
	return nil
}

// rebuildSet fills a temporary set and swaps it in, so the live set is never empty in between.
func rebuildSet(name string, f family, prefixes []netip.Prefix) error {
	tmp := name + "-tmp"
	_ = netlink.IpsetDestroy(tmp)
	if err := createSet(tmp, f, nil, true); err != nil {
		return err
	}
	defer func() { _ = netlink.IpsetDestroy(tmp) }()
	n := 0
	for _, p := range prefixes {
		if p.Addr().BitLen() != f.bits {
			continue
		}
		err := netlink.IpsetAdd(tmp, &netlink.IPSetEntry{IP: p.Addr().AsSlice(), CIDR: uint8(p.Bits())})
		if err != nil {
			return fmt.Errorf("ipset add %s %s: %w", tmp, p, err)
		}
		n++
	}
	if err := createSet(name, f, nil, false); err != nil {
		return err
	}
	if err := netlink.IpsetSwap(name, tmp); err != nil {
		return fmt.Errorf("ipset swap %s %s: %w", name, tmp, err)
	}
	slog.Debug("policyroute: rebuilt set", "set", name, "entries", n)
	return nil
}

// AddDnsEntry adds an address learned by the DNS forwarder; timeout is the clamped record TTL.
func (c *Controller) AddDnsEntry(kind domain.RoutingListKind, addr netip.Addr, timeout uint32) error {
	f := families[0]
	if addr.Is6() && !addr.Is4In6() {
		f = families[1]
	}
	addr = addr.Unmap()
	name := setName(kind, true, f)
	err := netlink.IpsetAdd(name, &netlink.IPSetEntry{
		IP: addr.AsSlice(), CIDR: uint8(f.bits), Timeout: &timeout, Replace: true,
	})
	if err != nil {
		return fmt.Errorf("ipset add %s %s timeout %d: %w", name, addr, timeout, err)
	}
	slog.Debug("policyroute: dns entry", "set", name, "addr", addr, "timeout", timeout)
	return nil
}

func (c *Controller) applyChains(ctx context.Context, f family, spec domain.PolicySpec) error {
	_ = c.ipt(ctx, f, "-t", "filter", "-N", chainForward) // fails if it exists
	_ = c.ipt(ctx, f, "-t", "mangle", "-N", chainMark)
	// shortcut: flush and re-add leaves a sub-second window without rules; use iptables-restore if that matters
	if err := c.ipt(ctx, f, "-t", "filter", "-F", chainForward); err != nil {
		return err
	}
	if err := c.ipt(ctx, f, "-t", "mangle", "-F", chainMark); err != nil {
		return err
	}
	for _, pi := range spec.Interfaces {
		for _, dns := range []bool{false, true} {
			block := setName(domain.RoutingListBlackhole, dns, f)
			rules := [][]string{
				{"-t", "filter", "-A", chainForward, "-i", pi.Name, "-p", "tcp", "-m", "set", "--match-set", block,
					"dst", "-j", "REJECT", "--reject-with", "tcp-reset"},
				{"-t", "filter", "-A", chainForward, "-i", pi.Name, "-m", "set", "--match-set", block, "dst",
					"-j", "REJECT", "--reject-with", f.icmpDeny},
				{"-t", "mangle", "-A", chainMark, "-i", pi.Name, "-m", "set", "--match-set",
					setName(domain.RoutingListBypass, dns, f), "dst", "-j", "MARK", "--set-xmark", markMask},
			}
			for _, r := range rules {
				if err := c.ipt(ctx, f, r...); err != nil {
					return err
				}
			}
		}
	}
	if err := c.ensureJump(ctx, f, "filter", "FORWARD", chainForward); err != nil {
		return err
	}
	return c.ensureJump(ctx, f, "mangle", "PREROUTING", chainMark)
}

// ensureJump keeps "-j target" as the first rule of chain, also when a PostUp hook inserted rules before it.
func (c *Controller) ensureJump(ctx context.Context, f family, table, chain, target string) error {
	if first, err := c.firstRule(ctx, f, table, chain); err == nil && first == "-A "+chain+" -j "+target {
		return nil
	}
	for range 10 {
		if c.ipt(ctx, f, "-t", table, "-D", chain, "-j", target) != nil {
			break
		}
	}
	return c.ipt(ctx, f, "-t", table, "-I", chain, "1", "-j", target)
}

func (c *Controller) firstRule(ctx context.Context, f family, table, chain string) (string, error) {
	out, err := exec.CommandContext(ctx, f.ipt, "-w", "-t", table, "-S", chain).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s -t %s -S %s: %w: %s", f.ipt, table, chain, err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "-A ") {
			return strings.TrimSpace(line), nil
		}
	}
	return "", nil
}

type ruleKey struct {
	family int
	pref   int
	iif    string
	src    string
	table  int
	mark   uint32
}

func keyOf(r netlink.Rule, fam int) ruleKey {
	src := ""
	if r.Src != nil {
		src = r.Src.String()
	}
	return ruleKey{family: fam, pref: r.Priority, iif: r.IifName, src: src, table: r.Table, mark: r.Mark}
}

func isOurPref(p int) bool {
	return p == domain.RulePrefBypass || p == domain.RulePrefProbe || p == domain.RulePrefExit
}

func (c *Controller) desiredRules(spec domain.PolicySpec, links map[string]netlink.Link) []netlink.Rule {
	var rules []netlink.Rule
	mask := uint32(domain.PolicyBypassMark)
	for _, pi := range spec.Interfaces {
		for _, f := range families {
			r := netlink.NewRule()
			r.Family, r.Priority, r.IifName, r.Table = f.nl, domain.RulePrefBypass, pi.Name, unix.RT_TABLE_MAIN
			r.Mark, r.Mask = domain.PolicyBypassMark, &mask
			rules = append(rules, *r)
		}
		for _, src := range pi.ProbeSources {
			r := netlink.NewRule()
			r.Family, r.Priority, r.IifName, r.Table = families[0].nl, domain.RulePrefProbe, pi.Name, unix.RT_TABLE_MAIN
			if src.Is6() {
				r.Family = families[1].nl
			}
			r.Src = &net.IPNet{IP: src.AsSlice(), Mask: net.CIDRMask(src.BitLen(), src.BitLen())}
			rules = append(rules, *r)
		}
		link, ok := links[pi.Name]
		if !pi.Exit || !ok {
			continue
		}
		for _, f := range families {
			if f.nl == netlink.FAMILY_V6 && !pi.V6 {
				continue
			}
			r := netlink.NewRule()
			r.Family, r.Priority, r.IifName = f.nl, domain.RulePrefExit, pi.Name
			r.Table = domain.ExitRoutingTable(link.Attrs().Index)
			rules = append(rules, *r)
		}
	}
	return rules
}

// applyRules adds missing and removes stale rules of our priorities.
func (c *Controller) applyRules(spec domain.PolicySpec, links map[string]netlink.Link) error {
	want := map[ruleKey]netlink.Rule{}
	for _, r := range c.desiredRules(spec, links) {
		want[keyOf(r, r.Family)] = r
	}
	for _, f := range families {
		existing, err := netlink.RuleList(f.nl)
		if ipv6Unavailable(f, err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("ip rule list: %w", err)
		}
		for _, r := range existing {
			if !isOurPref(r.Priority) {
				continue
			}
			k := keyOf(r, f.nl)
			if _, ok := want[k]; ok {
				delete(want, k) // already present
				continue
			}
			r.Family = f.nl // RuleList does not populate it
			if err := netlink.RuleDel(&r); err != nil {
				return fmt.Errorf("ip rule del %s: %w", r, err)
			}
			slog.Debug("policyroute: removed rule", "rule", r.String())
		}
	}
	for _, r := range want {
		if err := netlink.RuleAdd(&r); err != nil && !ipv6Unavailable(families[1], err, r.Family) {
			return fmt.Errorf("ip rule add %s: %w", r.String(), err)
		}
		slog.Debug("policyroute: added rule", "rule", r.String())
	}
	return nil
}

func defaultDst(f family) *net.IPNet {
	return &net.IPNet{IP: make(net.IP, f.bits/8), Mask: net.CIDRMask(0, f.bits)}
}

// applyExitRoutes installs "default dev X" in the exit table of each interface with an active exit and
// removes every other route in the exit table range.
func (c *Controller) applyExitRoutes(spec domain.PolicySpec, links map[string]netlink.Link) error {
	type routeKey struct{ family, table, link int }
	want := map[routeKey]bool{}
	for _, pi := range spec.Interfaces {
		link, ok := links[pi.Name]
		if !pi.Exit || !ok {
			continue
		}
		for _, f := range families {
			if f.nl == netlink.FAMILY_V6 && !pi.V6 {
				continue
			}
			idx := link.Attrs().Index
			route := &netlink.Route{LinkIndex: idx, Dst: defaultDst(f), Table: domain.ExitRoutingTable(idx),
				Scope: unix.RT_SCOPE_LINK, Family: f.nl}
			if err := netlink.RouteReplace(route); err != nil {
				return fmt.Errorf("ip route replace default dev %s table %d: %w", pi.Name, route.Table, err)
			}
			want[routeKey{f.nl, route.Table, idx}] = true
			slog.Debug("policyroute: exit route", "interface", pi.Name, "table", route.Table, "family", f.nl)
		}
	}
	return removeExitRoutes(func(r netlink.Route, fam int) bool { return want[routeKey{fam, r.Table, r.LinkIndex}] })
}

func removeExitRoutes(keep func(r netlink.Route, fam int) bool) error {
	for _, f := range families {
		routes, err := netlink.RouteListFiltered(f.nl, &netlink.Route{Table: unix.RT_TABLE_UNSPEC},
			netlink.RT_FILTER_TABLE)
		if ipv6Unavailable(f, err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("ip route list all tables: %w", err)
		}
		for _, r := range routes {
			if !domain.IsExitRoutingTable(r.Table) || keep(r, f.nl) {
				continue
			}
			if r.Dst == nil {
				r.Dst = defaultDst(f) // RouteDel needs a destination
			}
			if err := netlink.RouteDel(&r); err != nil {
				return fmt.Errorf("ip route del %s table %d: %w", r, r.Table, err)
			}
			slog.Debug("policyroute: removed exit route", "route", r.String(), "table", r.Table)
		}
	}
	return nil
}

// ipv6Unavailable reports (and logs) an IPv6 netlink error, which only disables the IPv6 part of the policy
// on hosts booted without IPv6. The optional ruleFamily restricts the check to rules of that family.
func ipv6Unavailable(f family, err error, ruleFamily ...int) bool {
	if err == nil || f.nl != netlink.FAMILY_V6 || (len(ruleFamily) > 0 && ruleFamily[0] != f.nl) {
		return false
	}
	slog.Warn("policyroute: IPv6 netlink operation failed, IPv6 is not policy routed", "error", err)
	return true
}

// endregion apply

// Verify checks the parts other programs may change: our jumps must stay first (PostUp hooks insert at
// position 1) and the exit routes vanish when an interface is re-created.
func (c *Controller) Verify(ctx context.Context, spec domain.PolicySpec) error {
	if !spec.Enabled() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, j := range [][3]string{{"filter", "FORWARD", chainForward}, {"mangle", "PREROUTING", chainMark}} {
		first, err := c.firstRule(ctx, families[0], j[0], j[1])
		if err != nil {
			return err
		}
		if first != "-A "+j[1]+" -j "+j[2] {
			return fmt.Errorf("%s %s does not start with our jump: %q", j[0], j[1], first)
		}
	}
	for _, pi := range spec.Interfaces {
		link, err := netlink.LinkByName(pi.Name)
		if err != nil {
			continue // Apply skipped it as well
		}
		table := domain.ExitRoutingTable(link.Attrs().Index)
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: table},
			netlink.RT_FILTER_TABLE)
		if err != nil {
			return err
		}
		if pi.Exit != (len(routes) > 0) {
			return fmt.Errorf("exit table %d of %s does not match (exit active: %v)", table, pi.Name, pi.Exit)
		}
	}
	return nil
}

// cleanup removes everything Apply may have added. Errors only mean there was nothing to remove (or the
// host has no iptables/ipset support), so they are logged at debug level.
func (c *Controller) cleanup(ctx context.Context) {
	for _, f := range families {
		for range 10 {
			if c.ipt(ctx, f, "-t", "filter", "-D", "FORWARD", "-j", chainForward) != nil {
				break
			}
		}
		for range 10 {
			if c.ipt(ctx, f, "-t", "mangle", "-D", "PREROUTING", "-j", chainMark) != nil {
				break
			}
		}
		_ = c.ipt(ctx, f, "-t", "filter", "-F", chainForward)
		_ = c.ipt(ctx, f, "-t", "filter", "-X", chainForward)
		_ = c.ipt(ctx, f, "-t", "mangle", "-F", chainMark)
		_ = c.ipt(ctx, f, "-t", "mangle", "-X", chainMark)
	}
	if err := c.applyRules(domain.PolicySpec{}, nil); err != nil {
		slog.Debug("policyroute: rule cleanup", "error", err)
	}
	if err := removeExitRoutes(func(netlink.Route, int) bool { return false }); err != nil {
		slog.Debug("policyroute: exit route cleanup", "error", err)
	}
	for _, f := range families {
		for _, kind := range []domain.RoutingListKind{domain.RoutingListBlackhole, domain.RoutingListBypass} {
			for _, dns := range []bool{false, true} {
				name := setName(kind, dns, f)
				for _, n := range []string{name, name + "-tmp"} {
					if err := netlink.IpsetDestroy(n); err == nil {
						slog.Debug("policyroute: destroyed set", "set", n)
					}
				}
			}
		}
	}
	c.staticHash = [32]byte{}
}

// ipt runs one iptables/ip6tables command; -w waits for the xtables lock.
func (c *Controller) ipt(ctx context.Context, f family, args ...string) error {
	args = append([]string{"-w"}, args...)
	out, err := exec.CommandContext(ctx, f.ipt, args...).CombinedOutput()
	cmd := f.ipt + " " + strings.Join(args, " ")
	if err != nil { // debug only: some failures are expected (chain exists, nothing to delete)
		slog.Debug("policyroute: command failed", "cmd", cmd, "output", strings.TrimSpace(string(out)))
		return fmt.Errorf("%s: %w: %s", cmd, err, strings.TrimSpace(string(out)))
	}
	slog.Debug("policyroute: applied", "cmd", cmd)
	return nil
}
