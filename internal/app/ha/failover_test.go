package ha

import (
	"testing"
	"time"

	"github.com/h44z/wg-portal/internal/config"
	"github.com/h44z/wg-portal/internal/domain"
)

func TestDecideFailover(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-5 * time.Second)
	stale := now.Add(-5 * time.Minute)
	cfg := config.HaConfig{FailThreshold: 3, HeartbeatTimeout: time.Minute, MinFailoverInterval: 5 * time.Minute}
	healthy := func(id string) domain.HaNode {
		return domain.HaNode{NodeId: id, LastProbeOk: true, LastHeartbeat: &fresh}
	}
	failing := func(id string, fails int) domain.HaNode {
		return domain.HaNode{NodeId: id, ConsecutiveFails: fails, LastHeartbeat: &fresh}
	}
	old := now.Add(-time.Hour)

	tests := []struct {
		name  string
		state domain.HaState
		nodes []domain.HaNode
		want  string
	}{
		{"bootstrap picks highest priority", domain.HaState{}, []domain.HaNode{healthy("a"), healthy("b")}, "a"},
		{"bootstrap skips unhealthy", domain.HaState{}, []domain.HaNode{failing("a", 1), healthy("b")}, "b"},
		{"healthy active is kept", domain.HaState{ActiveNodeId: "b", ChangedAt: old},
			[]domain.HaNode{healthy("a"), healthy("b")}, ""},
		{"below threshold is kept", domain.HaState{ActiveNodeId: "a", ChangedAt: old},
			[]domain.HaNode{failing("a", 2), healthy("b")}, ""},
		{"failed active is replaced", domain.HaState{ActiveNodeId: "a", ChangedAt: old},
			[]domain.HaNode{failing("a", 3), healthy("b")}, "b"},
		{"flap guard", domain.HaState{ActiveNodeId: "a", ChangedAt: now.Add(-time.Minute)},
			[]domain.HaNode{failing("a", 3), healthy("b")}, ""},
		{"stale heartbeat is no candidate", domain.HaState{ActiveNodeId: "a", ChangedAt: old},
			[]domain.HaNode{failing("a", 3), {NodeId: "b", LastProbeOk: true, LastHeartbeat: &stale}}, ""},
		{"no candidate keeps failed active", domain.HaState{ActiveNodeId: "a", ChangedAt: old},
			[]domain.HaNode{failing("a", 3), failing("b", 3)}, ""},
		{"removed active is replaced", domain.HaState{ActiveNodeId: "x", ChangedAt: now},
			[]domain.HaNode{healthy("a")}, "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := decideFailover(tt.state, tt.nodes, now, cfg)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
