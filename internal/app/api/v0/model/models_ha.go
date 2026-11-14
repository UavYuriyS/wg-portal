package model

import (
	"time"

	"github.com/h44z/wg-portal/internal/domain"
)

type HaNode struct {
	NodeId           string     `json:"NodeId"`
	PublicIp         string     `json:"PublicIp"`
	Priority         int        `json:"Priority"`
	RegisteredAt     time.Time  `json:"RegisteredAt"`
	LastHeartbeat    *time.Time `json:"LastHeartbeat"`
	LastProbeAt      *time.Time `json:"LastProbeAt"`
	LastProbeOk      bool       `json:"LastProbeOk"`
	LastProbeResult  string     `json:"LastProbeResult"`
	ConsecutiveFails int        `json:"ConsecutiveFails"`
	IsActive         bool       `json:"IsActive"`
}

type HaJoinToken struct {
	Id        string    `json:"Id"`
	CreatedBy string    `json:"CreatedBy"`
	CreatedAt time.Time `json:"CreatedAt"`
	ExpiresAt time.Time `json:"ExpiresAt"`
	UsesLeft  int       `json:"UsesLeft"`
}

// HaStatus never contains the cluster CA.
type HaStatus struct {
	Role         string        `json:"Role"`
	ActiveNodeId string        `json:"ActiveNodeId"`
	ExitCheck    HaExitCheck   `json:"ExitCheck"`
	ChangedAt    time.Time     `json:"ChangedAt"`
	ChangedBy    string        `json:"ChangedBy"`
	Nodes        []HaNode      `json:"Nodes"`
	JoinTokens   []HaJoinToken `json:"JoinTokens"`
}

// HaExitCheck is the informational probe of the active exit node through the active node. All fields are
// empty while no exit is active.
type HaExitCheck struct {
	At         *time.Time `json:"At"`
	Ok         bool       `json:"Ok"`
	Result     string     `json:"Result"`
	ExpectedIp string     `json:"ExpectedIp"`
}

type HaJoinTokenRequest struct {
	ValidHours int `json:"ValidHours"`
	Uses       int `json:"Uses"`
}

type HaJoinTokenResponse struct {
	Token string `json:"Token"`
}

func NewHaStatus(role string, state domain.HaState, nodes []domain.HaNode, tokens []domain.HaJoinToken) HaStatus {
	s := HaStatus{
		Role:         role,
		ActiveNodeId: state.ActiveNodeId,
		ExitCheck: HaExitCheck{At: state.ExitCheckAt, Ok: state.ExitCheckOk, Result: state.ExitCheckResult,
			ExpectedIp: state.ExitCheckExpectedIp},
		ChangedAt:  state.ChangedAt,
		ChangedBy:  state.ChangedBy,
		Nodes:      make([]HaNode, len(nodes)),
		JoinTokens: make([]HaJoinToken, len(tokens)),
	}
	for i, n := range nodes {
		s.Nodes[i] = HaNode{
			NodeId:           n.NodeId,
			PublicIp:         n.PublicIp,
			Priority:         n.Priority,
			RegisteredAt:     n.RegisteredAt,
			LastHeartbeat:    n.LastHeartbeat,
			LastProbeAt:      n.LastProbeAt,
			LastProbeOk:      n.LastProbeOk,
			LastProbeResult:  n.LastProbeResult,
			ConsecutiveFails: n.ConsecutiveFails,
			IsActive:         n.NodeId == state.ActiveNodeId,
		}
	}
	for i, t := range tokens {
		s.JoinTokens[i] = HaJoinToken{
			Id: t.Id, CreatedBy: t.CreatedBy, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, UsesLeft: t.UsesLeft,
		}
	}
	return s
}
