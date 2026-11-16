package ha

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/h44z/wg-portal/internal/config"
	"github.com/h44z/wg-portal/internal/domain"
)

// region dependencies

type Repo interface {
	GetHaState(ctx context.Context) (*domain.HaState, error)
	SaveHaState(ctx context.Context, updateFunc func(s *domain.HaState) error) error
	GetHaNodes(ctx context.Context) ([]domain.HaNode, error)
	GetHaNode(ctx context.Context, id string) (*domain.HaNode, error)
	SaveHaNode(ctx context.Context, node *domain.HaNode) error
	TouchHaNodeHeartbeat(ctx context.Context, id string, at time.Time) error
	UpdateHaNodeProbe(ctx context.Context, id string, ok bool, result string, at time.Time) error
	DeleteHaNode(ctx context.Context, id string) error
	GetHaJoinTokens(ctx context.Context) ([]domain.HaJoinToken, error)
	SaveHaJoinToken(ctx context.Context, token *domain.HaJoinToken) error
	DeleteHaJoinToken(ctx context.Context, id string) error
	ConsumeHaJoinToken(ctx context.Context, tokenHash string, now time.Time) error
	GetHaProbePeers(ctx context.Context) ([]domain.HaProbePeer, error)
	SaveHaProbePeer(ctx context.Context, probe *domain.HaProbePeer) error
	GetHaPeerSessions(ctx context.Context, nodeId string) ([]domain.HaPeerSession, error)
	ReplaceHaPeerSessions(ctx context.Context, nodeId string, sessions []domain.HaPeerSession) error

	GetAllInterfaces(ctx context.Context) ([]domain.Interface, error)
	GetPeer(ctx context.Context, id domain.PeerIdentifier) (*domain.Peer, error)
	SaveAuditEntry(ctx context.Context, entry *domain.AuditEntry) error
}

type PeerCreator interface {
	PreparePeer(ctx context.Context, id domain.InterfaceIdentifier) (*domain.Peer, error)
	CreatePeer(ctx context.Context, peer *domain.Peer) (*domain.Peer, error)
}

// ExitResolver returns the active exit candidate of an interface, nil if none is active.
type ExitResolver interface {
	ActiveExit(ctx context.Context, id domain.InterfaceIdentifier) (*domain.ExitCandidate, error)
}

// endregion dependencies

const (
	probePeerName     = "ha-probe (used by the HA orchestrator, do not delete)"
	exitProbePeerName = "ha-probe-exit (used by the HA orchestrator, do not delete)"
)

type Manager struct {
	cfg   *config.Config
	repo  Repo
	peers PeerCreator
	exits ExitResolver
	dns   DnsUpdater
	probe Prober
	now   func() time.Time

	active    atomic.Bool // node role: this node is the active one
	promoteMu sync.Mutex
}

func NewManager(cfg *config.Config, repo Repo, peers PeerCreator, exits ExitResolver) *Manager {
	m := &Manager{cfg: cfg, repo: repo, peers: peers, exits: exits, probe: WireGuardProbe, now: time.Now}
	if cfg.Ha.Dns.RegRu.Zone != "" {
		m.dns = NewRegRuDns(cfg.Ha.Dns.RegRu)
	}
	return m
}

// IsActiveNode is used to gate the statistics collector on nodes.
func (m *Manager) IsActiveNode() bool { return m.active.Load() }

// StartBackgroundJobs starts the role specific loops. It is a no-op for standalone instances.
func (m *Manager) StartBackgroundJobs(ctx context.Context) {
	ctx = domain.SetUserInfo(ctx, domain.SystemAdminContextUserInfo())
	switch m.cfg.Ha.Role {
	case config.HaRoleNode:
		go m.every(ctx, m.cfg.Ha.HeartbeatInterval, m.heartbeat)
	case config.HaRoleOrchestrator:
		if err := m.ensureCaInDb(ctx); err != nil {
			slog.Error("ha: failed to store cluster CA in database", "error", err)
		}
		go m.every(ctx, m.cfg.Ha.ProbeInterval, m.probeAndFailover)
	}
}

func (m *Manager) every(ctx context.Context, interval time.Duration, fn func(ctx context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// region node

func (m *Manager) heartbeat(ctx context.Context) {
	if err := m.repo.TouchHaNodeHeartbeat(ctx, m.cfg.Ha.NodeId, m.now()); err != nil {
		slog.Warn("ha: heartbeat failed", "node", m.cfg.Ha.NodeId, "error", err)
	}
	m.refreshMembers(ctx)
	state, err := m.repo.GetHaState(ctx)
	if err != nil {
		return // keep the last known role while the DB is unavailable
	}
	isActive := state.ActiveNodeId == m.cfg.Ha.NodeId
	if m.active.Swap(isActive) != isActive {
		slog.Info("ha: node role changed", "node", m.cfg.Ha.NodeId, "active", isActive)
	}
}

// refreshMembers adds nodes that joined after this one to its members file and galera.cnf. Galera reads
// wsrep_cluster_address only at startup, so a node restarting while the orchestrator is down must know
// the other nodes to rejoin the cluster.
// shortcut: members are only added (the orchestrator's address is not in the database); a removed node
// stays listed, which Galera skips as unreachable. Prune once node addresses change often.
func (m *Manager) refreshMembers(ctx context.Context) {
	nodes, err := m.repo.GetHaNodes(ctx)
	if err != nil {
		return
	}
	members, err := readMembers(m.cfg.Ha.DataDir)
	if err != nil {
		slog.Warn("ha: failed to read members file", "error", err)
		return
	}
	known := len(members)
	for _, n := range nodes {
		if n.PublicIp != "" && !slices.Contains(members, n.PublicIp) {
			members = append(members, n.PublicIp)
		}
	}
	if len(members) == known {
		return
	}
	cfg := *m.cfg // writeGaleraConfig reloads the node fields into the config it gets
	if err := writeMembers(cfg.Ha.DataDir, members); err != nil {
		slog.Warn("ha: failed to update members file", "error", err)
	} else if err := writeGaleraConfig(&cfg, members); err != nil {
		slog.Warn("ha: failed to update galera.cnf", "error", err)
	} else {
		slog.Info("ha: cluster members updated", "members", members)
	}
}

// endregion node

// region orchestrator

func (m *Manager) ensureCaInDb(ctx context.Context) error {
	certPem, err := os.ReadFile(caCertFile(m.cfg.Ha.DataDir))
	if err != nil {
		return fmt.Errorf("run wg-portal -ha-init first: %w", err)
	}
	keyPem, err := os.ReadFile(caKeyFile(m.cfg.Ha.DataDir))
	if err != nil {
		return err
	}
	return m.repo.SaveHaState(ctx, func(s *domain.HaState) error {
		s.CaCertPem, s.CaKeyPem = string(certPem), string(keyPem)
		return nil
	})
}

func (m *Manager) probeAndFailover(ctx context.Context) {
	if err := m.ensureProbePeers(ctx); err != nil {
		slog.Warn("ha: failed to create probe peers", "error", err)
	}
	nodes, err := m.repo.GetHaNodes(ctx)
	if err != nil {
		slog.Warn("ha: failed to load nodes", "error", err)
		return
	}
	state, err := m.repo.GetHaState(ctx)
	if err != nil {
		slog.Warn("ha: failed to load state", "error", err)
		return
	}
	target, _, err := m.probeTarget(ctx, func(p domain.HaProbePeer) domain.PeerIdentifier { return p.PeerIdentifier })
	if err != nil {
		slog.Debug("ha: probing disabled", "reason", err)
		return // without an interface there is nothing to fail over
	}

	var wg sync.WaitGroup
	for _, node := range nodes {
		wg.Add(1)
		go func(node domain.HaNode) {
			defer wg.Done()
			ok, result := m.probeVia(ctx, target, node.PublicIp, state.ExpectedEgressIp(node))
			if err := m.repo.UpdateHaNodeProbe(ctx, node.NodeId, ok, result, m.now()); err != nil {
				slog.Warn("ha: failed to store probe result", "node", node.NodeId, "error", err)
			}
		}(node)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.checkExit(ctx, *state, nodes)
	}()
	wg.Wait()

	if !m.cfg.Ha.AutoFailover {
		return
	}
	// decide on fresh state under the promote lock, so a manual promotion during the probes is never reverted
	m.promoteMu.Lock()
	defer m.promoteMu.Unlock()
	if nodes, err = m.repo.GetHaNodes(ctx); err != nil {
		return
	}
	if state, err = m.repo.GetHaState(ctx); err != nil {
		return
	}
	if next, reason := decideFailover(*state, nodes, m.now(), m.cfg.Ha); next != "" {
		slog.Warn("ha: automatic failover", "from", state.ActiveNodeId, "to", next, "reason", reason)
		if err := m.promoteLocked(ctx, next, "auto: "+reason); err != nil {
			slog.Error("ha: automatic failover failed", "to", next, "error", err)
		}
	}
}

// checkExit probes through the active node with the exit probe peer, expecting the active exit's EgressIp.
// It is informational only and never triggers a failover. shortcut: only the first server interface with
// probe peers is checked, like the node probe; check every interface when multi-interface HA is needed.
func (m *Manager) checkExit(ctx context.Context, state domain.HaState, nodes []domain.HaNode) {
	var active *domain.HaNode
	for i := range nodes {
		if nodes[i].NodeId == state.ActiveNodeId {
			active = &nodes[i]
		}
	}
	var exit *domain.ExitCandidate
	target, ifaceId, err := m.probeTarget(ctx,
		func(p domain.HaProbePeer) domain.PeerIdentifier { return p.ExitPeerIdentifier })
	if err == nil && m.exits != nil {
		exit, err = m.exits.ActiveExit(ctx, ifaceId)
	}
	if err != nil || exit == nil || active == nil {
		if state.ExitCheckAt == nil && state.ExitCheckExpectedIp == "" {
			return // already cleared
		}
		err = m.repo.SaveHaState(ctx, func(s *domain.HaState) error {
			s.ExitCheckAt, s.ExitCheckOk, s.ExitCheckResult, s.ExitCheckExpectedIp = nil, false, "", ""
			return nil
		})
		if err != nil {
			slog.Warn("ha: failed to clear exit check", "error", err)
		}
		return
	}

	ok, result := m.probeVia(ctx, target, active.PublicIp, exit.EgressIp)
	if !ok {
		slog.Warn("ha: exit check failed", "exit", exit.PeerIdentifier, "via", active.NodeId, "result", result)
	}
	now := m.now()
	err = m.repo.SaveHaState(ctx, func(s *domain.HaState) error {
		s.ExitCheckAt, s.ExitCheckOk, s.ExitCheckResult, s.ExitCheckExpectedIp = &now, ok, result, exit.EgressIp
		return nil
	})
	if err != nil {
		slog.Warn("ha: failed to store exit check", "error", err)
	}
}

// probeVia requests the probe URL through the node at nodeIp and compares the egress IP with expected.
func (m *Manager) probeVia(ctx context.Context, target ProbeTarget, nodeIp, expected string) (bool, string) {
	target.Endpoint = net.JoinHostPort(nodeIp, target.Endpoint)
	ctx, cancel := context.WithTimeout(ctx, m.cfg.Ha.ProbeTimeout)
	defer cancel()
	got, err := m.probe(ctx, target)
	switch {
	case err != nil:
		return false, err.Error()
	case got != expected:
		return false, fmt.Sprintf("egress %s, expected %s", got, expected)
	default:
		return true, got
	}
}

// probeTarget builds the probe parameters from the first server interface that has the picked probe peer.
// Endpoint only holds the listen port, probeVia prefixes the node IP.
func (m *Manager) probeTarget(ctx context.Context, pick func(domain.HaProbePeer) domain.PeerIdentifier) (
	ProbeTarget,
	domain.InterfaceIdentifier,
	error,
) {
	probes, err := m.repo.GetHaProbePeers(ctx)
	if err != nil {
		return ProbeTarget{}, "", err
	}
	interfaces, err := m.repo.GetAllInterfaces(ctx)
	if err != nil {
		return ProbeTarget{}, "", err
	}
	for _, probe := range probes {
		for _, iface := range interfaces {
			if iface.Identifier != probe.InterfaceIdentifier || iface.IsDisabled() || pick(probe) == "" {
				continue
			}
			peer, err := m.repo.GetPeer(ctx, pick(probe))
			if err != nil || len(peer.Interface.Addresses) == 0 {
				continue
			}
			addr, err := netip.ParseAddr(peer.Interface.Addresses[0].Addr)
			if err != nil {
				continue
			}
			return ProbeTarget{
				Endpoint:        strconv.Itoa(iface.ListenPort),
				ServerPublicKey: iface.PublicKey,
				ClientKeys:      peer.Interface.KeyPair,
				PresharedKey:    peer.PresharedKey,
				ClientAddress:   addr,
				ProbeUrl:        m.cfg.Ha.ProbeUrl,
			}, iface.Identifier, nil
		}
	}
	return ProbeTarget{}, "", errors.New("no enabled server interface with a probe peer")
}

// ensureProbePeers creates the node probe peer and the exit probe peer for every server interface that
// misses one.
func (m *Manager) ensureProbePeers(ctx context.Context) error {
	probes, err := m.repo.GetHaProbePeers(ctx)
	if err != nil {
		return err
	}
	byIface := map[domain.InterfaceIdentifier]domain.HaProbePeer{}
	for _, p := range probes {
		byIface[p.InterfaceIdentifier] = p
	}
	interfaces, err := m.repo.GetAllInterfaces(ctx)
	if err != nil {
		return err
	}
	for _, iface := range interfaces {
		if iface.Type != domain.InterfaceTypeServer {
			continue
		}
		probe := byIface[iface.Identifier]
		probe.InterfaceIdentifier = iface.Identifier
		changed := false
		for _, p := range []struct {
			id   *domain.PeerIdentifier
			name string
		}{{&probe.PeerIdentifier, probePeerName}, {&probe.ExitPeerIdentifier, exitProbePeerName}} {
			missing, err := m.peerMissing(ctx, *p.id)
			if err != nil {
				return err // a DB error must not create duplicate probe peers
			}
			if !missing {
				continue
			}
			if *p.id, err = m.createProbePeer(ctx, iface.Identifier, p.name); err != nil {
				return err
			}
			changed = true
		}
		if changed {
			if err := m.repo.SaveHaProbePeer(ctx, &probe); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) peerMissing(ctx context.Context, id domain.PeerIdentifier) (bool, error) {
	if id == "" {
		return true, nil
	}
	_, err := m.repo.GetPeer(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return true, nil
	}
	return false, err
}

func (m *Manager) createProbePeer(ctx context.Context, id domain.InterfaceIdentifier, name string) (
	domain.PeerIdentifier,
	error,
) {
	peer, err := m.peers.PreparePeer(ctx, id)
	if err != nil {
		return "", fmt.Errorf("prepare probe peer for %s: %w", id, err)
	}
	peer.DisplayName = name
	peer.UserIdentifier = ""
	peer.ExpiresAt = nil
	peer, err = m.peers.CreatePeer(ctx, peer)
	if err != nil {
		return "", fmt.Errorf("create probe peer for %s: %w", id, err)
	}
	slog.Info("ha: created probe peer", "interface", id, "peer", peer.Identifier, "name", name)
	return peer.Identifier, nil
}

// Promote makes nodeId the active node: DNS first, so a failed DNS update leaves the state unchanged
// and the next automatic run retries.
func (m *Manager) Promote(ctx context.Context, nodeId, actor string) error {
	m.promoteMu.Lock()
	defer m.promoteMu.Unlock()
	return m.promoteLocked(ctx, nodeId, actor)
}

func (m *Manager) promoteLocked(ctx context.Context, nodeId, actor string) error {
	node, err := m.repo.GetHaNode(ctx, nodeId)
	if err != nil {
		return fmt.Errorf("node %s: %w", nodeId, err)
	}
	nodes, err := m.repo.GetHaNodes(ctx)
	if err != nil {
		return err
	}
	var oldIps []string
	for _, n := range nodes {
		if n.NodeId != nodeId {
			oldIps = append(oldIps, n.PublicIp)
		}
	}

	if m.dns != nil {
		err := m.dns.SetA(ctx, node.PublicIp, oldIps)
		var partial *PartialDnsError
		switch {
		case errors.As(err, &partial):
			slog.Warn("ha: new A record added, but old records remain", "error", err)
		case err != nil:
			return fmt.Errorf("dns update: %w", err)
		}
	}

	var previous string
	err = m.repo.SaveHaState(ctx, func(s *domain.HaState) error {
		previous = s.ActiveNodeId
		s.ActiveNodeId = nodeId
		s.ChangedAt = m.now()
		s.ChangedBy = actor
		return nil
	})
	if err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	if m.cfg.Ha.OnPromote != "" {
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", m.cfg.Ha.OnPromote)
		cmd.Env = append(os.Environ(), "NODE_ID="+nodeId, "NODE_IP="+node.PublicIp, "PREVIOUS_NODE_ID="+previous)
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Error("ha: on_promote hook failed", "error", err, "output", string(out))
		}
	}

	_ = m.repo.SaveAuditEntry(ctx, &domain.AuditEntry{
		CreatedAt:   m.now(),
		ContextUser: actor,
		Severity:    domain.AuditSeverityLevelHigh,
		Origin:      "ha",
		Message:     fmt.Sprintf("HA: promoted node %s (%s), previous active: %q", nodeId, node.PublicIp, previous),
	})
	slog.Info("ha: node promoted", "node", nodeId, "ip", node.PublicIp, "previous", previous, "by", actor)
	return nil
}

// endregion orchestrator

// region admin api

type Status struct {
	State  domain.HaState
	Nodes  []domain.HaNode
	Tokens []domain.HaJoinToken
	// ConnectedPeers counts the fresh, connected sessions per node id, HA probe peers excluded.
	ConnectedPeers map[string]int
}

func (m *Manager) GetStatus(ctx context.Context) (*Status, error) {
	state, err := m.repo.GetHaState(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := m.repo.GetHaNodes(ctx)
	if err != nil {
		return nil, err
	}
	tokens, err := m.repo.GetHaJoinTokens(ctx)
	if err != nil {
		return nil, err
	}
	connected, err := m.connectedPeers(ctx)
	if err != nil {
		return nil, err
	}
	return &Status{State: *state, Nodes: nodes, Tokens: tokens, ConnectedPeers: connected}, nil
}

func (m *Manager) connectedPeers(ctx context.Context) (map[string]int, error) {
	probes, err := m.repo.GetHaProbePeers(ctx)
	if err != nil {
		return nil, err
	}
	sessions, err := m.repo.GetHaPeerSessions(ctx, "")
	if err != nil {
		return nil, err
	}
	connected := make(map[string]int)
	for _, s := range sessions {
		isProbe := slices.ContainsFunc(probes, func(p domain.HaProbePeer) bool {
			return p.PeerIdentifier == s.PeerIdentifier || p.ExitPeerIdentifier == s.PeerIdentifier
		})
		if !isProbe && s.Fresh(m.cfg.Statistics.DataCollectionInterval) &&
			s.PeerStatus(m.cfg.Backend.ReKeyTimeoutInterval).IsConnected {
			connected[s.NodeId]++
		}
	}
	return connected, nil
}

func (m *Manager) RemoveNode(ctx context.Context, nodeId string) error {
	state, err := m.repo.GetHaState(ctx)
	if err != nil {
		return err
	}
	if state.ActiveNodeId == nodeId {
		return fmt.Errorf("%w: promote another node before removing the active one", domain.ErrInvalidData)
	}
	if err := m.repo.DeleteHaNode(ctx, nodeId); err != nil {
		return err
	}
	return m.repo.ReplaceHaPeerSessions(ctx, nodeId, nil)
}

// CreateJoinToken returns the clear text token; only its hash is stored.
func (m *Manager) CreateJoinToken(ctx context.Context, validFor time.Duration, uses int) (string, error) {
	if uses < 1 {
		uses = 1
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw[:4])
	token := id + "." + hex.EncodeToString(raw[4:])
	err := m.repo.SaveHaJoinToken(ctx, &domain.HaJoinToken{
		Id:        id,
		TokenHash: hashToken(token),
		CreatedBy: string(domain.GetUserInfo(ctx).Id),
		CreatedAt: m.now(),
		ExpiresAt: m.now().Add(validFor),
		UsesLeft:  uses,
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func (m *Manager) DeleteJoinToken(ctx context.Context, id string) error {
	return m.repo.DeleteHaJoinToken(ctx, id)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// endregion admin api

// region registration

type RegisterRequest struct {
	NodeId   string `json:"NodeId"`
	PublicIp string `json:"PublicIp"`
	Priority int    `json:"Priority"`
}

type RegisterResponse struct {
	NodeId               string   `json:"NodeId"`
	ClusterName          string   `json:"ClusterName"`
	ClusterAddresses     []string `json:"ClusterAddresses"`
	CaCert               string   `json:"CaCert"`
	Cert                 string   `json:"Cert"`
	Key                  string   `json:"Key"`
	DatabaseDsn          string   `json:"DatabaseDsn"`
	EncryptionPassphrase string   `json:"EncryptionPassphrase"`
	SstUser              string   `json:"SstUser"`
	SstPassword          string   `json:"SstPassword"`
	ListenPorts          []int    `json:"ListenPorts"`
}

var ErrInvalidJoinToken = errors.New("invalid, expired or used up join token")

// Register adds a node to the cluster and returns everything it needs to join Galera.
func (m *Manager) Register(ctx context.Context, token string, req RegisterRequest) (*RegisterResponse, error) {
	if !m.cfg.Ha.IsOrchestrator() {
		return nil, fmt.Errorf("%w: not an orchestrator", domain.ErrInvalidData)
	}
	if req.NodeId == "" || net.ParseIP(req.PublicIp) == nil {
		return nil, fmt.Errorf("%w: NodeId and a valid PublicIp are required", domain.ErrInvalidData)
	}
	// orchestrator side checks first, so a misconfiguration does not use up the token
	state, err := m.repo.GetHaState(ctx)
	if err != nil {
		return nil, err
	}
	if state.CaKeyPem == "" {
		return nil, errors.New("cluster CA missing, run wg-portal -ha-init on the orchestrator")
	}
	dsn, err := localDsn(m.cfg.Database.DSN)
	if err != nil {
		return nil, err
	}

	if err := m.repo.ConsumeHaJoinToken(ctx, hashToken(token), m.now()); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, ErrInvalidJoinToken
		}
		return nil, err
	}
	cert, key, err := SignMemberCert(state.CaCertPem, state.CaKeyPem, req.NodeId, req.PublicIp)
	if err != nil {
		return nil, fmt.Errorf("sign node certificate: %w", err)
	}

	err = m.repo.SaveHaNode(ctx, &domain.HaNode{
		NodeId:       req.NodeId,
		PublicIp:     req.PublicIp,
		Priority:     req.Priority,
		RegisteredAt: m.now(),
	})
	if err != nil {
		return nil, err
	}
	nodes, err := m.repo.GetHaNodes(ctx)
	if err != nil {
		return nil, err
	}
	members := []string{m.cfg.Ha.PublicIp} // every Galera member: orchestrator + all nodes
	for _, n := range nodes {
		members = append(members, n.PublicIp)
	}
	// galera.cnf too: a plain container restart or host reboot does not re-run ha-init, and the bootstrap
	// config (no members) would then refuse to start
	if err := writeMembers(m.cfg.Ha.DataDir, members); err != nil {
		slog.Warn("ha: failed to update members file", "error", err)
	} else if err := writeGaleraConfig(m.cfg, members); err != nil {
		slog.Warn("ha: failed to update galera.cnf", "error", err)
	}

	var ports []int
	if interfaces, err := m.repo.GetAllInterfaces(ctx); err == nil {
		for _, iface := range interfaces {
			if iface.ListenPort != 0 {
				ports = append(ports, iface.ListenPort)
			}
		}
	}

	_ = m.repo.SaveAuditEntry(ctx, &domain.AuditEntry{
		CreatedAt: m.now(),
		Severity:  domain.AuditSeverityLevelHigh,
		Origin:    "ha",
		Message:   fmt.Sprintf("HA: node %s (%s) registered", req.NodeId, req.PublicIp),
	})

	return &RegisterResponse{
		NodeId:               req.NodeId,
		ClusterName:          m.cfg.Ha.Galera.ClusterName,
		ClusterAddresses:     members,
		CaCert:               state.CaCertPem,
		Cert:                 cert,
		Key:                  key,
		DatabaseDsn:          dsn,
		EncryptionPassphrase: m.cfg.Database.EncryptionPassphrase,
		SstUser:              m.cfg.Ha.Galera.SstUser,
		SstPassword:          m.cfg.Ha.Galera.SstPassword,
		ListenPorts:          ports,
	}, nil
}

// localDsn rewrites the orchestrator's MySQL DSN to point at the node's local Galera member.
func localDsn(dsn string) (string, error) {
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("orchestrator database.dsn must be a MySQL/MariaDB DSN: %w", err)
	}
	parsed.Net = "tcp"
	parsed.Addr = "127.0.0.1:3306"
	return parsed.FormatDSN(), nil
}

// endregion registration
