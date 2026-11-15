package routing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/h44z/wg-portal/internal/app"
	"github.com/h44z/wg-portal/internal/config"
	"github.com/h44z/wg-portal/internal/domain"
)

// region dependencies

type Repo interface {
	GetAllInterfaces(ctx context.Context) ([]domain.Interface, error)
	GetInterface(ctx context.Context, id domain.InterfaceIdentifier) (*domain.Interface, error)
	GetInterfacePeers(ctx context.Context, id domain.InterfaceIdentifier) ([]domain.Peer, error)
	GetPeer(ctx context.Context, id domain.PeerIdentifier) (*domain.Peer, error)
	GetExitCandidates(ctx context.Context, id domain.InterfaceIdentifier) ([]domain.ExitCandidate, error)
	ReplaceExitCandidates(ctx context.Context, id domain.InterfaceIdentifier, c []domain.ExitCandidate) error
	RenameExitCandidatePeer(ctx context.Context, oldId, newId domain.PeerIdentifier) error
	GetRoutingLists(ctx context.Context, withContent bool) ([]domain.RoutingList, error)
	GetRoutingList(ctx context.Context, id string) (*domain.RoutingList, error)
	SaveRoutingList(ctx context.Context, list *domain.RoutingList) error
	DeleteRoutingList(ctx context.Context, id string) error
	GetRoutingSettings(ctx context.Context) (*domain.RoutingSettings, error)
	SaveRoutingSettings(ctx context.Context, s *domain.RoutingSettings) error
	GetHaProbePeers(ctx context.Context) ([]domain.HaProbePeer, error)
	SaveAuditEntry(ctx context.Context, entry *domain.AuditEntry) error
}

type PeerUpdater interface {
	UpdatePeer(ctx context.Context, peer *domain.Peer) (*domain.Peer, error)
}

type EventBus interface {
	Publish(topic string, args ...any)
	Subscribe(topic string, fn any) error
}

// PolicyController applies the routing policy to the kernel (ipsets, iptables, ip rules, exit tables).
type PolicyController interface {
	DnsEntryAdder
	// Apply makes the kernel match spec. It is idempotent; a disabled spec removes everything it ever added.
	Apply(ctx context.Context, spec domain.PolicySpec) error
	// Verify cheaply checks that an applied spec is still in place (our jumps first, exit routes present).
	Verify(ctx context.Context, spec domain.PolicySpec) error
}

// endregion dependencies

type Manager struct {
	cfg    *config.Config
	bus    EventBus
	repo   Repo
	peers  PeerUpdater
	policy PolicyController
	now    func() time.Time
	kick   chan struct{}

	matcher     atomic.Pointer[domainMatcher]
	nextMatcher *domainMatcher // loaded lists, swapped in once their DNS sets exist
	dns         *dnsForwarder

	// apply loop state
	listsKey        string
	static          [2][]netip.Prefix // block, bypass
	domains         [2][]string       // block, bypass (block wins, so bypass holds no blocked domain)
	flushDns        []domain.RoutingListKind
	lastFingerprint [32]byte
	lastSpec        domain.PolicySpec
	exitModes       map[domain.InterfaceIdentifier]bool // as of the last apply
	warnedDetected  map[domain.PeerIdentifier]bool
}

func NewManager(cfg *config.Config, bus EventBus, repo Repo, peers PeerUpdater, policy PolicyController) *Manager {
	m := &Manager{cfg: cfg, bus: bus, repo: repo, peers: peers, policy: policy, now: time.Now,
		kick: make(chan struct{}, 1), exitModes: map[domain.InterfaceIdentifier]bool{},
		warnedDetected: map[domain.PeerIdentifier]bool{}}
	m.dns = newDnsForwarder(policy, &m.matcher)
	_ = bus.Subscribe(app.TopicPeerIdentifierUpdated, m.handlePeerIdentifierUpdated)
	return m
}

// handlePeerIdentifierUpdated keeps a rotated exit candidate a candidate.
func (m *Manager) handlePeerIdentifierUpdated(oldId, newId domain.PeerIdentifier) {
	ctx := domain.SetUserInfo(context.Background(), domain.SystemAdminContextUserInfo())
	if err := m.repo.RenameExitCandidatePeer(ctx, oldId, newId); err != nil {
		slog.Error("routing: failed to re-key exit candidate", "old", oldId, "new", newId, "error", err)
		return
	}
	m.Kick()
}

// Kick triggers an immediate apply in Run (a no-op on the orchestrator, where Run does not execute).
func (m *Manager) Kick() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// Run applies the routing policy immediately, then every interval and after each change until ctx is done.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	ctx = domain.SetUserInfo(ctx, domain.SystemAdminContextUserInfo())
	m.flushDns = []domain.RoutingListKind{domain.RoutingListBlackhole, domain.RoutingListBypass} // stale on start
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := m.Apply(ctx); err != nil {
			// a DB error (e.g. Galera non-primary) keeps the current kernel state untouched
			slog.Warn("routing: apply failed, keeping current policy", "error", err)
		}
		select {
		case <-ctx.Done():
			m.dns.Sync(nil, nil)
			return
		case <-ticker.C:
		case <-m.kick:
		}
	}
}

// Apply reads lists, settings, candidates, the active markers and probe peers and applies them. Nothing is
// done if the DB did not change and the kernel still matches.
func (m *Manager) Apply(ctx context.Context) error {
	if err := m.loadLists(ctx); err != nil {
		return err
	}
	settings, err := m.repo.GetRoutingSettings(ctx)
	if err != nil {
		return fmt.Errorf("load routing settings: %w", err)
	}
	spec, dnsAddrs, exitModes, err := m.buildSpec(ctx)
	if err != nil {
		return err
	}
	m.resyncRoutesOnExitModeChange(ctx, exitModes)

	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s|%+v", m.listsKey, spec.Interfaces)
	var fingerprint [32]byte
	copy(fingerprint[:], h.Sum(nil))
	verifyErr := errors.New("changed")
	if fingerprint == m.lastFingerprint && len(m.flushDns) == 0 {
		if verifyErr = m.policy.Verify(ctx, m.lastSpec); verifyErr != nil {
			slog.Info("routing: kernel state drifted, re-applying", "reason", verifyErr)
		}
	}
	var applyErr error
	if verifyErr != nil {
		spec.FlushDns = m.flushDns
		if applyErr = m.policy.Apply(ctx, spec); applyErr == nil {
			m.flushDns = nil
			m.lastFingerprint = fingerprint
			m.lastSpec = spec
			slog.Debug("routing: policy applied", "interfaces", len(spec.Interfaces), "block", len(spec.Block),
				"bypass", len(spec.Bypass))
		}
	}

	// the new lists only once their DNS sets exist; name resolution itself must never depend on the policy
	if applyErr == nil && m.nextMatcher != nil {
		m.matcher.Store(m.nextMatcher)
		m.nextMatcher = nil
	}
	var upstreams []string
	if settings.DnsForwarderEnabled {
		upstreams = splitUpstreams(settings.DnsUpstreams)
	} else {
		dnsAddrs = nil
	}
	m.dns.Sync(dnsAddrs, upstreams)
	if applyErr != nil {
		return fmt.Errorf("apply policy (DNS forwarder still serves, without list matching): %w", applyErr)
	}
	return nil
}

// loadLists re-parses the enabled lists when one of them changed.
func (m *Manager) loadLists(ctx context.Context) error {
	lists, err := m.repo.GetRoutingLists(ctx, false)
	if err != nil {
		return fmt.Errorf("load routing lists: %w", err)
	}
	key := "lists:" // never empty, so the first call always loads
	for _, l := range lists {
		if l.Enabled {
			key += fmt.Sprintf("%s|%s|%d;", l.Id, l.Kind, l.UpdatedAt.UnixNano())
		}
	}
	if key == m.listsKey {
		return nil
	}

	prefixes := [2]map[netip.Prefix]bool{{}, {}}
	domains := [2]map[string]bool{{}, {}}
	for _, meta := range lists {
		if !meta.Enabled {
			continue
		}
		l, err := m.repo.GetRoutingList(ctx, meta.Id)
		if err != nil {
			return fmt.Errorf("load routing list %s: %w", meta.Id, err)
		}
		parsed, err := ParseList(l.Content)
		if err != nil { // only possible for rows written around the API
			slog.Error("routing: skipping invalid list", "list", l.Name, "error", err)
			continue
		}
		k := kindIndex(l.Kind)
		for _, p := range parsed.Prefixes {
			prefixes[k][p] = true
		}
		for _, d := range parsed.Domains {
			domains[k][d] = true
		}
	}
	for d := range domains[0] {
		delete(domains[1], d) // blackhole wins
	}

	var next [2][]string
	for k := range 2 {
		m.static[k] = sortedKeys(prefixes[k], func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
		next[k] = sortedKeys(domains[k], strings.Compare)
		// learned addresses of removed domains must go; added domains need no flush
		if !isSubset(m.domains[k], next[k]) {
			m.flushDns = append(m.flushDns, []domain.RoutingListKind{domain.RoutingListBlackhole,
				domain.RoutingListBypass}[k])
		}
	}
	m.domains = next
	matcher := newDomainMatcher(next[0], next[1])
	m.nextMatcher = &matcher
	m.listsKey = key
	return nil
}

// resyncRoutesOnExitModeChange re-publishes the routes of interfaces that entered or left exit mode: the route
// manager strips default routes only in exit mode. HA nodes would otherwise keep a wg-quick style default route
// (which captures all server traffic) until the peer carrying it changes, because candidates are no peer change.
func (m *Manager) resyncRoutesOnExitModeChange(ctx context.Context, modes map[domain.InterfaceIdentifier]bool) {
	for id, on := range modes {
		if m.exitModes[id] == on {
			continue
		}
		if err := m.publishRoutes(ctx, id); err != nil {
			slog.Warn("routing: failed to re-sync routes after exit mode change", "interface", id, "error", err)
			continue // retried on the next apply
		}
		slog.Info("routing: exit mode changed, routes re-synced", "interface", id, "exitMode", on)
		m.exitModes[id] = on
	}
}

// buildSpec builds the desired kernel state and returns the addresses the DNS forwarder should listen on and the
// exit mode of each local server interface.
func (m *Manager) buildSpec(ctx context.Context) (
	domain.PolicySpec, []netip.Addr, map[domain.InterfaceIdentifier]bool, error,
) {
	interfaces, err := m.repo.GetAllInterfaces(ctx)
	if err != nil {
		return domain.PolicySpec{}, nil, nil, fmt.Errorf("load interfaces: %w", err)
	}
	probes, err := m.repo.GetHaProbePeers(ctx)
	if err != nil {
		return domain.PolicySpec{}, nil, nil, fmt.Errorf("load probe peers: %w", err)
	}

	var spec domain.PolicySpec
	var dnsAddrs []netip.Addr
	exitModes := map[domain.InterfaceIdentifier]bool{}
	enabled := len(m.static[0])+len(m.static[1])+len(m.domains[0])+len(m.domains[1]) > 0
	for _, iface := range interfaces {
		if iface.Type != domain.InterfaceTypeServer || iface.IsDisabled() ||
			(iface.Backend != "" && iface.Backend != config.LocalBackendName) {
			continue
		}
		for _, a := range iface.Addresses {
			if addr, err := netip.ParseAddr(a.Addr); err == nil {
				dnsAddrs = append(dnsAddrs, addr)
			}
		}
		candidates, err := m.repo.GetExitCandidates(ctx, iface.Identifier)
		if err != nil {
			return domain.PolicySpec{}, nil, nil, fmt.Errorf("load exit candidates of %s: %w", iface.Identifier, err)
		}
		peers, err := m.repo.GetInterfacePeers(ctx, iface.Identifier)
		if err != nil {
			return domain.PolicySpec{}, nil, nil, fmt.Errorf("load peers of %s: %w", iface.Identifier, err)
		}
		enabled = enabled || len(candidates) > 0
		exitModes[iface.Identifier] = len(candidates) > 0
		for _, p := range detectedExits(candidates, peers) {
			if m.warnedDetected[p.Identifier] {
				continue
			}
			m.warnedDetected[p.Identifier] = true
			if len(candidates) == 0 {
				slog.Warn("routing: peer has a default route but is no exit candidate; the route manager sends "+
					"ALL server traffic through it until it is added as an exit candidate on the routing page",
					"interface", iface.Identifier, "peer", p.Identifier, "name", p.DisplayName)
			} else {
				slog.Warn("routing: peer has a default route but is no exit candidate; it is ignored in exit mode",
					"interface", iface.Identifier, "peer", p.Identifier, "name", p.DisplayName)
			}
		}

		pi := domain.PolicyInterface{
			Name:     string(iface.Identifier),
			ExitMode: len(candidates) > 0,
			Exit:     activeExit(candidates, peers) != nil,
			V6:       interfaceHasV6(&iface),
			Table:    iface.GetRoutingTable(),
		}
		if pi.Table <= 0 {
			pi.Table = int(iface.FirewallMark) // the route manager uses the fwmark as table number
		}
		for _, probe := range probes {
			if probe.InterfaceIdentifier != iface.Identifier {
				continue
			}
			for _, p := range peers {
				if p.Identifier != probe.PeerIdentifier { // the exit probe peer is routed like any peer
					continue
				}
				for _, a := range p.Interface.Addresses {
					if addr, err := netip.ParseAddr(a.Addr); err == nil {
						pi.ProbeSources = append(pi.ProbeSources, addr)
					}
				}
			}
		}
		spec.Interfaces = append(spec.Interfaces, pi)
	}
	if !enabled { // feature off: no candidates and no lists
		return domain.PolicySpec{}, dnsAddrs, exitModes, nil
	}
	spec.Block, spec.Bypass = m.static[0], m.static[1]
	return spec, dnsAddrs, exitModes, nil
}

// region lists and settings api

func kindIndex(k domain.RoutingListKind) int {
	if k == domain.RoutingListBlackhole {
		return 0
	}
	return 1
}

type ListRequest struct {
	Name    string
	Kind    domain.RoutingListKind
	Enabled bool
	Content *string // nil keeps the current content on update
}

func (m *Manager) GetLists(ctx context.Context) ([]domain.RoutingList, error) {
	if err := domain.ValidateAdminAccessRights(ctx); err != nil {
		return nil, err
	}
	return m.repo.GetRoutingLists(ctx, false)
}

func (m *Manager) GetList(ctx context.Context, id string) (*domain.RoutingList, error) {
	if err := domain.ValidateAdminAccessRights(ctx); err != nil {
		return nil, err
	}
	return m.repo.GetRoutingList(ctx, id)
}

// SaveList creates (empty id) or updates a list. The content is validated as a whole.
func (m *Manager) SaveList(ctx context.Context, id string, req ListRequest) (*domain.RoutingList, error) {
	if err := domain.ValidateAdminAccessRights(ctx); err != nil {
		return nil, err
	}
	list := &domain.RoutingList{}
	if id != "" {
		existing, err := m.repo.GetRoutingList(ctx, id)
		if err != nil {
			return nil, err
		}
		list = existing
	} else {
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		list.Id = hex.EncodeToString(raw)
		if req.Content == nil {
			req.Content = new(string)
		}
	}
	list.Name = strings.TrimSpace(req.Name)
	if list.Name == "" {
		return nil, fmt.Errorf("%w: Name is required", domain.ErrInvalidData)
	}
	if req.Kind != domain.RoutingListBlackhole && req.Kind != domain.RoutingListBypass {
		return nil, fmt.Errorf("%w: Kind must be %q or %q", domain.ErrInvalidData, domain.RoutingListBlackhole,
			domain.RoutingListBypass)
	}
	list.Kind, list.Enabled = req.Kind, req.Enabled
	if req.Content != nil {
		parsed, err := ParseList(*req.Content)
		if err != nil {
			return nil, err
		}
		list.Content, list.EntryCount = *req.Content, parsed.Count()
	}
	list.UpdatedAt = m.now()
	list.UpdatedBy = string(domain.GetUserInfo(ctx).Id)
	if err := m.repo.SaveRoutingList(ctx, list); err != nil {
		return nil, err
	}
	m.audit(ctx, fmt.Sprintf("Routing: saved %s list %q (%d entries, enabled: %v)", list.Kind, list.Name,
		list.EntryCount, list.Enabled))
	m.Kick()
	list.Content = ""
	return list, nil
}

func (m *Manager) DeleteList(ctx context.Context, id string) error {
	if err := domain.ValidateAdminAccessRights(ctx); err != nil {
		return err
	}
	if err := m.repo.DeleteRoutingList(ctx, id); err != nil {
		return err
	}
	m.audit(ctx, fmt.Sprintf("Routing: deleted list %s", id))
	m.Kick()
	return nil
}

func (m *Manager) GetSettings(ctx context.Context) (*domain.RoutingSettings, error) {
	if err := domain.ValidateAdminAccessRights(ctx); err != nil {
		return nil, err
	}
	return m.repo.GetRoutingSettings(ctx)
}

func (m *Manager) SaveSettings(ctx context.Context, enabled bool, upstreams []string) (*domain.RoutingSettings,
	error) {
	if err := domain.ValidateAdminAccessRights(ctx); err != nil {
		return nil, err
	}
	var normalized []string
	for _, u := range upstreams {
		if strings.TrimSpace(u) == "" {
			continue
		}
		n, ok := normalizeUpstream(u)
		if !ok {
			return nil, fmt.Errorf("%w: invalid upstream %q, expected host[:port]", domain.ErrInvalidData, u)
		}
		normalized = append(normalized, n)
	}
	if enabled && len(normalized) == 0 {
		return nil, fmt.Errorf("%w: the DNS forwarder needs at least one upstream", domain.ErrInvalidData)
	}
	s := &domain.RoutingSettings{DnsForwarderEnabled: enabled, DnsUpstreams: strings.Join(normalized, ","),
		UpdatedAt: m.now()}
	if err := m.repo.SaveRoutingSettings(ctx, s); err != nil {
		return nil, err
	}
	m.audit(ctx, fmt.Sprintf("Routing: DNS forwarder enabled: %v, upstreams: %s", enabled, s.DnsUpstreams))
	m.Kick()
	return s, nil
}

// endregion lists and settings api

func (m *Manager) audit(ctx context.Context, msg string) {
	_ = m.repo.SaveAuditEntry(ctx, &domain.AuditEntry{
		CreatedAt:   m.now(),
		ContextUser: string(domain.GetUserInfo(ctx).Id),
		Severity:    domain.AuditSeverityLevelHigh,
		Origin:      "routing",
		Message:     msg,
	})
}

func splitUpstreams(s string) []string {
	var out []string
	for _, u := range strings.Split(s, ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}

func sortedKeys[K comparable](set map[K]bool, cmp func(a, b K) int) []K {
	keys := make([]K, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, cmp)
	return keys
}

// isSubset reports whether every element of sorted a is in sorted b.
func isSubset(a, b []string) bool {
	for _, x := range a {
		if _, found := slices.BinarySearch(b, x); !found {
			return false
		}
	}
	return true
}
