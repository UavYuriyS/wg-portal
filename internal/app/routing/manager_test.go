package routing

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/schema"

	"github.com/h44z/wg-portal/internal/adapters"
	"github.com/h44z/wg-portal/internal/app"
	"github.com/h44z/wg-portal/internal/config"
	"github.com/h44z/wg-portal/internal/domain"
)

func init() {
	schema.RegisterSerializer("encstr", app.NewGormEncryptedStringSerializer("test-passphrase"))
}

type fakePolicy struct{ specs []domain.PolicySpec }

func (f *fakePolicy) Apply(_ context.Context, s domain.PolicySpec) error {
	f.specs = append(f.specs, s)
	return nil
}
func (f *fakePolicy) Verify(context.Context, domain.PolicySpec) error { return nil }
func (f *fakePolicy) AddDnsEntry(domain.RoutingListKind, netip.Addr, uint32) error {
	return nil
}

type fakeBus struct{ routeUpdates []domain.InterfaceIdentifier }

func (b *fakeBus) Publish(topic string, args ...any) {
	if info, ok := args[0].(domain.RoutingTableInfo); ok && topic == app.TopicRouteUpdate {
		b.routeUpdates = append(b.routeUpdates, info.Interface.Identifier)
	}
}

// peerSaver mimics the WireGuard manager: validate, then save.
type peerSaver struct {
	repo     *adapters.SqlRepo
	validate func(ctx context.Context, p *domain.Peer) error
	updates  []domain.PeerIdentifier
}

func (s *peerSaver) UpdatePeer(ctx context.Context, p *domain.Peer) (*domain.Peer, error) {
	if err := s.validate(ctx, p); err != nil {
		return nil, err
	}
	s.updates = append(s.updates, p.Identifier)
	return p, s.repo.SavePeer(ctx, p.Identifier, func(*domain.Peer) (*domain.Peer, error) { return p, nil })
}

func newTestManager(t *testing.T) (*Manager, *adapters.SqlRepo, *fakePolicy, *peerSaver) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Database = config.DatabaseConfig{Type: config.DatabaseSQLite, DSN: filepath.Join(t.TempDir(), "db.sqlite")}
	db, err := adapters.NewDatabase(cfg.Database)
	require.NoError(t, err)
	repo, err := adapters.NewSqlRepository(db, cfg)
	require.NoError(t, err)
	policy := &fakePolicy{}
	saver := &peerSaver{repo: repo}
	m := NewManager(cfg, &fakeBus{}, repo, saver, policy)
	saver.validate = m.ValidatePeer
	return m, repo, policy, saver
}

func cidrs(s string) []domain.Cidr {
	c, _ := domain.CidrsFromString(s)
	return c
}

func savePeer(t *testing.T, repo *adapters.SqlRepo, id, addr, extra string) {
	t.Helper()
	p := &domain.Peer{Identifier: domain.PeerIdentifier(id), InterfaceIdentifier: "wg0", DisplayName: id,
		ExtraAllowedIPsStr: extra}
	p.Interface.Addresses = cidrs(addr)
	require.NoError(t, repo.SavePeer(context.Background(), p.Identifier,
		func(*domain.Peer) (*domain.Peer, error) { return p, nil }))
}

func peerExtra(t *testing.T, repo *adapters.SqlRepo, id string) string {
	p, err := repo.GetPeer(context.Background(), domain.PeerIdentifier(id))
	require.NoError(t, err)
	return p.ExtraAllowedIPsStr
}

func TestExitSelectionAndApply(t *testing.T) {
	m, repo, policy, saver := newTestManager(t)
	ctx := domain.SetUserInfo(context.Background(), domain.SystemAdminContextUserInfo())

	require.NoError(t, repo.SaveInterface(ctx, "wg0", func(*domain.Interface) (*domain.Interface, error) {
		return &domain.Interface{Identifier: "wg0", Type: domain.InterfaceTypeServer, Backend: "local",
			Addresses: cidrs("10.11.12.1/24")}, nil
	}))
	savePeer(t, repo, "client", "10.11.12.2/32", "")
	savePeer(t, repo, "exit-a", "10.11.12.20/32", "")
	savePeer(t, repo, "exit-b", "10.11.12.21/32", "192.168.50.0/24")
	savePeer(t, repo, "probe", "10.11.12.30/32", "")
	require.NoError(t, repo.SaveHaProbePeer(ctx, &domain.HaProbePeer{InterfaceIdentifier: "wg0",
		PeerIdentifier: "probe", ExitPeerIdentifier: "client"}))

	// feature off: zero spec
	require.NoError(t, m.Apply(ctx))
	require.Len(t, policy.specs, 1)
	assert.False(t, policy.specs[0].Enabled())

	_, err := m.SetExitCandidates(ctx, "wg0", []domain.ExitCandidate{{PeerIdentifier: "exit-a", EgressIp: "bad"}})
	assert.True(t, errors.Is(err, domain.ErrInvalidData))
	info, err := m.SetExitCandidates(ctx, "wg0", []domain.ExitCandidate{
		{PeerIdentifier: "exit-a", EgressIp: "192.0.2.20"}, {PeerIdentifier: "exit-b", EgressIp: "192.0.2.21"}})
	require.NoError(t, err)
	assert.Len(t, info.Candidates, 2)
	assert.Empty(t, info.ActivePeerIdentifier)

	// only candidates may carry the default route on an exit-mode interface
	err = m.ValidatePeer(ctx, &domain.Peer{Identifier: "client", InterfaceIdentifier: "wg0",
		ExtraAllowedIPsStr: "0.0.0.0/0"})
	assert.True(t, errors.Is(err, domain.ErrInvalidData))

	info, err = m.SetActiveExit(ctx, "wg0", "exit-a")
	require.NoError(t, err)
	assert.Equal(t, domain.PeerIdentifier("exit-a"), info.ActivePeerIdentifier)
	assert.Equal(t, "0.0.0.0/0", peerExtra(t, repo, "exit-a")) // no ::/0, wg0 has no IPv6

	require.NoError(t, m.Apply(ctx))
	spec := policy.specs[len(policy.specs)-1]
	require.Len(t, spec.Interfaces, 1)
	assert.Equal(t, domain.PolicyInterface{Name: "wg0", ExitMode: true, Exit: true,
		ProbeSources: []netip.Addr{netip.MustParseAddr("10.11.12.30")}}, spec.Interfaces[0])

	saver.updates = nil
	_, err = m.SetActiveExit(ctx, "wg0", "exit-b")
	require.NoError(t, err)
	assert.Equal(t, []domain.PeerIdentifier{"exit-a", "exit-b"}, saver.updates) // loser first
	assert.Equal(t, "", peerExtra(t, repo, "exit-a"))
	assert.Equal(t, "192.168.50.0/24,0.0.0.0/0", peerExtra(t, repo, "exit-b"))

	_, err = m.SetActiveExit(ctx, "wg0", "client")
	assert.True(t, errors.Is(err, domain.ErrInvalidData))

	// removing the active exit from the candidates clears the marker
	info, err = m.SetExitCandidates(ctx, "wg0", []domain.ExitCandidate{{PeerIdentifier: "exit-a",
		EgressIp: "192.0.2.20"}})
	require.NoError(t, err)
	assert.Empty(t, info.ActivePeerIdentifier)
	assert.Equal(t, "192.168.50.0/24", peerExtra(t, repo, "exit-b"))

	// lists: invalid content is rejected, valid content reaches the spec; blackhole wins for domains
	_, err = m.SaveList(ctx, "", ListRequest{Name: "x", Kind: domain.RoutingListBypass, Content: ptr("not valid!")})
	assert.True(t, errors.Is(err, domain.ErrInvalidData))
	block, err := m.SaveList(ctx, "", ListRequest{Name: "block", Kind: domain.RoutingListBlackhole, Enabled: true,
		Content: ptr("192.0.2.58\nboth.test\n")})
	require.NoError(t, err)
	assert.Equal(t, 2, block.EntryCount)
	_, err = m.SaveList(ctx, "", ListRequest{Name: "bypass", Kind: domain.RoutingListBypass, Enabled: true,
		Content: ptr("192.0.2.57\ndirect.test\nboth.test\n")})
	require.NoError(t, err)

	require.NoError(t, m.Apply(ctx))
	spec = policy.specs[len(policy.specs)-1]
	assert.False(t, spec.Interfaces[0].Exit)
	assert.Equal(t, []netip.Prefix{netip.MustParsePrefix("192.0.2.58/32")}, spec.Block)
	assert.Equal(t, []netip.Prefix{netip.MustParsePrefix("192.0.2.57/32")}, spec.Bypass)
	kind, _ := m.matcher.Load().match("www.both.test.")
	assert.Equal(t, domain.RoutingListBlackhole, kind)

	// disabling a list (content kept) drops its domains and flushes their learned addresses
	_, err = m.SaveList(ctx, block.Id, ListRequest{Name: "block", Kind: domain.RoutingListBlackhole})
	require.NoError(t, err)
	require.NoError(t, m.Apply(ctx))
	spec = policy.specs[len(policy.specs)-1]
	assert.Empty(t, spec.Block)
	assert.Equal(t, []domain.RoutingListKind{domain.RoutingListBlackhole}, spec.FlushDns)
	stored, _ := repo.GetRoutingList(ctx, block.Id)
	assert.Equal(t, "192.0.2.58\nboth.test\n", stored.Content)

	// unchanged DB: nothing is re-applied
	n := len(policy.specs)
	require.NoError(t, m.Apply(ctx))
	assert.Len(t, policy.specs, n)
}

// A peer that got 0.0.0.0/0 before exit nodes existed is reported as detected, and adopting it as a candidate
// re-syncs the routes in the apply loop (HA nodes see no peer change in that case).
func TestDetectedExitAdoption(t *testing.T) {
	m, repo, _, _ := newTestManager(t)
	bus := &fakeBus{}
	m.bus = bus
	ctx := domain.SetUserInfo(context.Background(), domain.SystemAdminContextUserInfo())

	require.NoError(t, repo.SaveInterface(ctx, "wg0", func(*domain.Interface) (*domain.Interface, error) {
		return &domain.Interface{Identifier: "wg0", Type: domain.InterfaceTypeServer, Backend: "local",
			Addresses: cidrs("10.11.12.1/24")}, nil
	}))
	savePeer(t, repo, "client", "10.11.12.2/32", "")
	savePeer(t, repo, "legacy", "10.11.12.20/32", "0.0.0.0/0")

	exits, err := m.GetExits(ctx)
	require.NoError(t, err)
	require.Len(t, exits, 1)
	assert.Empty(t, exits[0].ActivePeerIdentifier)
	assert.Equal(t, []ExitCandidateInfo{{PeerIdentifier: "legacy", DisplayName: "legacy"}}, exits[0].Detected)

	require.NoError(t, m.Apply(ctx))
	assert.Empty(t, bus.routeUpdates) // not in exit mode: nothing to re-sync

	// adopted as a candidate: it is the active exit and no longer "detected"
	require.NoError(t, repo.ReplaceExitCandidates(ctx, "wg0", []domain.ExitCandidate{{InterfaceIdentifier: "wg0",
		PeerIdentifier: "legacy", EgressIp: "192.0.2.20"}})) // as replicated to an HA node, without the API
	exits, err = m.GetExits(ctx)
	require.NoError(t, err)
	assert.Equal(t, domain.PeerIdentifier("legacy"), exits[0].ActivePeerIdentifier)
	assert.Empty(t, exits[0].Detected)

	require.NoError(t, m.Apply(ctx))
	assert.Equal(t, []domain.InterfaceIdentifier{"wg0"}, bus.routeUpdates) // entered exit mode
	require.NoError(t, m.Apply(ctx))
	assert.Len(t, bus.routeUpdates, 1) // unchanged: no second re-sync
}

func TestMoveExitMarker(t *testing.T) {
	peers := []domain.Peer{
		{Identifier: "a", ExtraAllowedIPsStr: "0.0.0.0/0,::/0"},
		{Identifier: "b", ExtraAllowedIPsStr: "10.0.0.0/24"},
		{Identifier: "c", ExtraAllowedIPsStr: ""},
		{Identifier: "d", ExtraAllowedIPsStr: "not a cidr"},
	}
	changed := moveExitMarker(peers, "b", true)
	require.Len(t, changed, 2)
	assert.Equal(t, domain.PeerIdentifier("a"), changed[0].Identifier) // loses first
	assert.Equal(t, "", changed[0].ExtraAllowedIPsStr)
	assert.Equal(t, "10.0.0.0/24,0.0.0.0/0,::/0", changed[1].ExtraAllowedIPsStr)

	changed = moveExitMarker(peers, "c", false)
	assert.Equal(t, "0.0.0.0/0", changed[len(changed)-1].ExtraAllowedIPsStr) // no ::/0 without IPv6

	changed = moveExitMarker(peers, "", true)
	require.Len(t, changed, 1)
	assert.Equal(t, domain.PeerIdentifier("a"), changed[0].Identifier)
}

func ptr(s string) *string { return &s }
