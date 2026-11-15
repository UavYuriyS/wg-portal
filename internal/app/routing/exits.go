package routing

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/h44z/wg-portal/internal/app"
	"github.com/h44z/wg-portal/internal/domain"
)

type ExitCandidateInfo struct {
	PeerIdentifier domain.PeerIdentifier
	DisplayName    string
	EgressIp       string
}

type ExitInfo struct {
	InterfaceIdentifier  domain.InterfaceIdentifier
	ActivePeerIdentifier domain.PeerIdentifier // empty: no exit active
	Candidates           []ExitCandidateInfo
	// Detected are peers that carry 0.0.0.0/0 or ::/0 but are no candidates, e.g. from before exit nodes
	// existed. Without candidates the route manager installs that default route for ALL server traffic.
	Detected []ExitCandidateInfo
}

// detectedExits returns the peers that carry the default route marker without being exit candidates.
func detectedExits(candidates []domain.ExitCandidate, peers []domain.Peer) []domain.Peer {
	var detected []domain.Peer
	for _, p := range peers {
		if domain.HasDefaultRoute(p.ExtraAllowedIPsStr) &&
			!slices.ContainsFunc(candidates, func(c domain.ExitCandidate) bool { return c.PeerIdentifier == p.Identifier }) {
			detected = append(detected, p)
		}
	}
	return detected
}

// activeExit returns the candidate that carries the default route marker, if any.
func activeExit(candidates []domain.ExitCandidate, peers []domain.Peer) *domain.ExitCandidate {
	for _, p := range peers {
		if !domain.HasDefaultRoute(p.ExtraAllowedIPsStr) {
			continue
		}
		for i := range candidates {
			if candidates[i].PeerIdentifier == p.Identifier {
				return &candidates[i]
			}
		}
	}
	return nil
}

// moveExitMarker removes 0.0.0.0/0 and ::/0 from the extra allowed IPs of all peers and adds them to the
// selected peer (::/0 only if the interface has IPv6). It returns the changed peers, losers first, so the
// default route is never owned twice.
func moveExitMarker(peers []domain.Peer, selected domain.PeerIdentifier, v6 bool) []domain.Peer {
	var losers, winners []domain.Peer
	for _, p := range peers {
		cidrs, err := domain.CidrsFromString(p.ExtraAllowedIPsStr)
		if err != nil && p.ExtraAllowedIPsStr != "" && p.Identifier != selected {
			continue // unparsable, so it carries no marker the kernel would use; never rewrite it
		}
		next := domain.StripDefaultRoutes(cidrs)
		if p.Identifier == selected {
			next = append(next, domain.CidrFromPrefix(netip.MustParsePrefix("0.0.0.0/0")))
			if v6 {
				next = append(next, domain.CidrFromPrefix(netip.MustParsePrefix("::/0")))
			}
		}
		nextStr := domain.CidrsToString(next)
		if nextStr == p.ExtraAllowedIPsStr {
			continue
		}
		p.ExtraAllowedIPsStr = nextStr
		if p.Identifier == selected {
			winners = append(winners, p)
		} else {
			losers = append(losers, p)
		}
	}
	return append(losers, winners...)
}

func interfaceHasV6(iface *domain.Interface) bool {
	return slices.ContainsFunc(iface.Addresses, func(c domain.Cidr) bool { return !c.IsV4() })
}

func (m *Manager) serverInterface(ctx context.Context, id domain.InterfaceIdentifier) (*domain.Interface, error) {
	iface, err := m.repo.GetInterface(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("interface %s: %w", id, err)
	}
	if iface.Type != domain.InterfaceTypeServer {
		return nil, fmt.Errorf("%w: %s is not a server interface", domain.ErrInvalidData, id)
	}
	return iface, nil
}

func (m *Manager) exitInfo(ctx context.Context, id domain.InterfaceIdentifier) (*ExitInfo, error) {
	candidates, err := m.repo.GetExitCandidates(ctx, id)
	if err != nil {
		return nil, err
	}
	peers, err := m.repo.GetInterfacePeers(ctx, id)
	if err != nil {
		return nil, err
	}
	info := &ExitInfo{InterfaceIdentifier: id, Candidates: []ExitCandidateInfo{}, Detected: []ExitCandidateInfo{}}
	for _, p := range detectedExits(candidates, peers) {
		info.Detected = append(info.Detected, ExitCandidateInfo{PeerIdentifier: p.Identifier, DisplayName: p.DisplayName})
	}
	if active := activeExit(candidates, peers); active != nil {
		info.ActivePeerIdentifier = active.PeerIdentifier
	}
	for _, c := range candidates {
		ci := ExitCandidateInfo{PeerIdentifier: c.PeerIdentifier, EgressIp: c.EgressIp}
		for _, p := range peers {
			if p.Identifier == c.PeerIdentifier {
				ci.DisplayName = p.DisplayName
			}
		}
		info.Candidates = append(info.Candidates, ci)
	}
	return info, nil
}

// GetExits returns the exit configuration of all server interfaces.
func (m *Manager) GetExits(ctx context.Context) ([]ExitInfo, error) {
	if err := domain.ValidateAdminAccessRights(ctx); err != nil {
		return nil, err
	}
	interfaces, err := m.repo.GetAllInterfaces(ctx)
	if err != nil {
		return nil, err
	}
	exits := []ExitInfo{}
	for _, iface := range interfaces {
		if iface.Type != domain.InterfaceTypeServer {
			continue
		}
		info, err := m.exitInfo(ctx, iface.Identifier)
		if err != nil {
			return nil, err
		}
		exits = append(exits, *info)
	}
	return exits, nil
}

// ActiveExit returns the active exit candidate of an interface, or nil if none is active.
func (m *Manager) ActiveExit(ctx context.Context, id domain.InterfaceIdentifier) (*domain.ExitCandidate, error) {
	candidates, err := m.repo.GetExitCandidates(ctx, id)
	if err != nil || len(candidates) == 0 {
		return nil, err
	}
	peers, err := m.repo.GetInterfacePeers(ctx, id)
	if err != nil {
		return nil, err
	}
	return activeExit(candidates, peers), nil
}

// SetExitCandidates replaces the exit candidates of an interface. Removing the active exit clears the marker.
func (m *Manager) SetExitCandidates(
	ctx context.Context,
	id domain.InterfaceIdentifier,
	candidates []domain.ExitCandidate,
) (*ExitInfo, error) {
	if err := domain.ValidateAdminAccessRights(ctx); err != nil {
		return nil, err
	}
	if _, err := m.serverInterface(ctx, id); err != nil {
		return nil, err
	}
	seen := map[domain.PeerIdentifier]bool{}
	for i, c := range candidates {
		if seen[c.PeerIdentifier] {
			return nil, fmt.Errorf("%w: duplicate candidate %s", domain.ErrInvalidData, c.PeerIdentifier)
		}
		seen[c.PeerIdentifier] = true
		peer, err := m.repo.GetPeer(ctx, c.PeerIdentifier)
		if err != nil {
			return nil, fmt.Errorf("peer %s: %w", c.PeerIdentifier, err)
		}
		if peer.InterfaceIdentifier != id {
			return nil, fmt.Errorf("%w: peer %s does not belong to %s", domain.ErrInvalidData, c.PeerIdentifier, id)
		}
		if _, err := netip.ParseAddr(c.EgressIp); err != nil {
			return nil, fmt.Errorf("%w: invalid EgressIp %q of %s", domain.ErrInvalidData, c.EgressIp,
				c.PeerIdentifier)
		}
		candidates[i].InterfaceIdentifier = id
		candidates[i].CreatedAt = m.now()
	}

	active, err := m.ActiveExit(ctx, id)
	if err != nil {
		return nil, err
	}
	if active != nil && !seen[active.PeerIdentifier] {
		if err := m.setActive(ctx, id, ""); err != nil {
			return nil, err
		}
	}
	if err := m.repo.ReplaceExitCandidates(ctx, id, candidates); err != nil {
		return nil, err
	}
	m.audit(ctx, fmt.Sprintf("Routing: set %d exit candidate(s) for %s", len(candidates), id))
	if err := m.publishRoutes(ctx, id); err != nil {
		return nil, err
	}
	m.Kick()
	return m.exitInfo(ctx, id)
}

// publishRoutes re-syncs the interface routes, so entering exit mode drops an already installed default route.
func (m *Manager) publishRoutes(ctx context.Context, id domain.InterfaceIdentifier) error {
	iface, err := m.repo.GetInterface(ctx, id)
	if err != nil {
		return err
	}
	peers, err := m.repo.GetInterfacePeers(ctx, id)
	if err != nil {
		return err
	}
	m.bus.Publish(app.TopicRouteUpdate, domain.RoutingTableInfo{
		Interface:  *iface,
		AllowedIps: iface.GetAllowedIPs(peers),
		FwMark:     iface.FirewallMark,
		Table:      iface.GetRoutingTable(),
		TableStr:   iface.RoutingTable,
	})
	return nil
}

// SetActiveExit moves the default route marker to the given candidate; an empty peer means no exit.
func (m *Manager) SetActiveExit(ctx context.Context, id domain.InterfaceIdentifier, peer domain.PeerIdentifier) (
	*ExitInfo,
	error,
) {
	if err := domain.ValidateAdminAccessRights(ctx); err != nil {
		return nil, err
	}
	if peer != "" {
		candidates, err := m.repo.GetExitCandidates(ctx, id)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(candidates, func(c domain.ExitCandidate) bool { return c.PeerIdentifier == peer }) {
			return nil, fmt.Errorf("%w: peer %s is not an exit candidate of %s", domain.ErrInvalidData, peer, id)
		}
	}
	if err := m.setActive(ctx, id, peer); err != nil {
		return nil, err
	}
	m.Kick()
	return m.exitInfo(ctx, id)
}

// setActive saves the moved marker through the WireGuard manager, so a standalone instance applies it to
// the kernel at once, the orchestrator only writes the DB and HA nodes pick it up on reconcile.
func (m *Manager) setActive(ctx context.Context, id domain.InterfaceIdentifier, peer domain.PeerIdentifier) error {
	iface, err := m.serverInterface(ctx, id)
	if err != nil {
		return err
	}
	peers, err := m.repo.GetInterfacePeers(ctx, id)
	if err != nil {
		return err
	}
	for _, p := range moveExitMarker(peers, peer, interfaceHasV6(iface)) {
		if _, err := m.peers.UpdatePeer(ctx, &p); err != nil {
			return fmt.Errorf("update peer %s: %w", p.Identifier, err)
		}
	}
	if peer == "" {
		m.audit(ctx, fmt.Sprintf("Routing: no exit node active on %s", id))
	} else {
		m.audit(ctx, fmt.Sprintf("Routing: exit node of %s is now %s", id, peer))
	}
	return nil
}

// ValidatePeer rejects adding a default route to the extra allowed IPs of a peer of an exit-mode interface unless
// the peer is an exit candidate. It is a hook of the WireGuard manager.
func (m *Manager) ValidatePeer(ctx context.Context, peer *domain.Peer) error {
	if !domain.HasDefaultRoute(peer.ExtraAllowedIPsStr) {
		return nil
	}
	candidates, err := m.repo.GetExitCandidates(ctx, peer.InterfaceIdentifier)
	if err != nil {
		return err
	}
	if len(candidates) == 0 ||
		slices.ContainsFunc(candidates, func(c domain.ExitCandidate) bool { return c.PeerIdentifier == peer.Identifier }) {
		return nil
	}
	// a peer that already had it (from before exit mode, listed as detected) must stay editable
	existing, err := m.repo.GetPeer(ctx, peer.Identifier)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if existing != nil && domain.HasDefaultRoute(existing.ExtraAllowedIPsStr) {
		return nil
	}
	return fmt.Errorf("%w: %s is in exit mode, only exit candidates may have 0.0.0.0/0 or ::/0 in their "+
		"extra allowed IPs, select the exit node on the routing page", domain.ErrInvalidData, peer.InterfaceIdentifier)
}
