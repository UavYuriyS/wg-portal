package domain

import (
	"net/netip"
	"time"
)

// ExitCandidate is a peer of a server interface that may become the exit node of that interface. An
// interface with at least one candidate is in "exit mode", see docs/documentation/getting-started/routing-policy.md.
type ExitCandidate struct {
	InterfaceIdentifier InterfaceIdentifier `gorm:"primaryKey"`
	PeerIdentifier      PeerIdentifier      `gorm:"primaryKey"`
	EgressIp            string              // public IP the exit host egresses with, checked by the HA exit check
	CreatedAt           time.Time
}

type RoutingListKind string

const (
	RoutingListBlackhole RoutingListKind = "blackhole"
	RoutingListBypass    RoutingListKind = "bypass"
)

// RoutingList is an uploaded list of IPs, CIDRs and domains that are blackholed or bypass the exit node.
type RoutingList struct {
	Id         string `gorm:"primaryKey"` // random hex, no auto increment (Galera)
	Name       string
	Kind       RoutingListKind
	Enabled    bool
	Content    string
	EntryCount int
	UpdatedAt  time.Time
	UpdatedBy  string
}

// RoutingSettings is the single-row routing configuration (Id is always 1).
type RoutingSettings struct {
	Id                  int `gorm:"primaryKey"`
	DnsForwarderEnabled bool
	DnsUpstreams        string // comma separated host[:port]
	UpdatedAt           time.Time
}

const RoutingSettingsId = 1

// Policy routing constants, shared by the policy controller and the route manager.
const (
	// ExitRoutingTableBase + link index is the exit table of an interface. wg-portal's own tables are
	// advanced.route_table_offset (default 20000) + link index, far below this range.
	ExitRoutingTableBase  = 0x5700_0000
	exitRoutingTableRange = 1 << 24

	PolicyBypassMark = 0x10000000 // fwmark bit set on bypassed traffic

	RulePrefBypass = 5200 // iif X fwmark bypass lookup main
	RulePrefProbe  = 5210 // iif X from <HA probe peer> lookup main
	RulePrefExit   = 5220 // iif X lookup <exit table>
)

func ExitRoutingTable(linkIndex int) int { return ExitRoutingTableBase + linkIndex }

func IsExitRoutingTable(table int) bool {
	return table >= ExitRoutingTableBase && table < ExitRoutingTableBase+exitRoutingTableRange
}

// PolicySpec is the desired kernel state of the routing policy. A zero spec removes everything.
type PolicySpec struct {
	Interfaces []PolicyInterface
	Block      []netip.Prefix // static blackhole entries
	Bypass     []netip.Prefix // static bypass entries
	// FlushDns lists the kinds whose DNS learned entries must be dropped (domains were removed).
	FlushDns []RoutingListKind
}

func (s PolicySpec) Enabled() bool { return len(s.Interfaces) > 0 }

type PolicyInterface struct {
	Name         string
	ExitMode     bool         // the interface has exit candidates, so the route manager strips default routes
	Exit         bool         // an exit is active: route everything not bypassed into the exit table
	V6           bool         // the interface has IPv6, so the exit table gets ::/0 too
	ProbeSources []netip.Addr // HA node probe peer addresses, they never use the exit
	Table        int          // the interface's own routing table (route manager), must not collide
}

// HasDefaultRoute reports whether a comma separated CIDR list contains 0.0.0.0/0 or ::/0.
func HasDefaultRoute(cidrs string) bool {
	parsed, _ := CidrsFromString(cidrs)
	return ContainsDefaultRoute(parsed)
}

// StripDefaultRoutes returns cidrs without 0.0.0.0/0 and ::/0.
func StripDefaultRoutes(cidrs []Cidr) []Cidr {
	out := make([]Cidr, 0, len(cidrs))
	for _, c := range cidrs {
		if c.NetLength != 0 {
			out = append(out, c)
		}
	}
	return out
}
