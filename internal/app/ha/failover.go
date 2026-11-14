package ha

import (
	"time"

	"github.com/h44z/wg-portal/internal/config"
	"github.com/h44z/wg-portal/internal/domain"
)

// isCandidate reports whether a node may become active: its last probe succeeded and its wg-portal is
// alive (a fresh heartbeat means its kernel state follows the database).
func isCandidate(n domain.HaNode, now time.Time, cfg config.HaConfig) bool {
	return n.LastProbeOk && n.LastHeartbeat != nil && now.Sub(*n.LastHeartbeat) < cfg.HeartbeatTimeout
}

// decideFailover returns the node that should become active, or "" to keep the current state.
// Nodes must be ordered by priority, highest first. Fail-back never happens automatically: a healthy
// active node is always kept.
func decideFailover(state domain.HaState, nodes []domain.HaNode, now time.Time, cfg config.HaConfig) (string, string) {
	var active *domain.HaNode
	for i := range nodes {
		if nodes[i].NodeId == state.ActiveNodeId {
			active = &nodes[i]
		}
	}

	reason := ""
	switch {
	case state.ActiveNodeId == "":
		reason = "no active node"
	case active == nil:
		reason = "active node was removed"
	case active.ConsecutiveFails >= cfg.FailThreshold:
		if now.Sub(state.ChangedAt) < cfg.MinFailoverInterval {
			return "", "" // flap guard
		}
		reason = "active node failed probes"
	default:
		return "", ""
	}

	for _, n := range nodes {
		if n.NodeId != state.ActiveNodeId && isCandidate(n, now, cfg) {
			return n.NodeId, reason
		}
	}
	return "", ""
}
