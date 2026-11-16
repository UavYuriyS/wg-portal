package domain

import "time"

// HaNode is one WireGuard node of the HA cluster. Rows are replicated to every party.
type HaNode struct {
	NodeId           string `gorm:"primaryKey"`
	PublicIp         string
	Priority         int
	RegisteredAt     time.Time
	LastHeartbeat    *time.Time
	LastProbeAt      *time.Time
	LastProbeOk      bool
	LastProbeResult  string // observed egress IP or the probe error
	ConsecutiveFails int
}

// HaState is the single-row cluster state (Id is always 1).
type HaState struct {
	Id           int `gorm:"primaryKey"`
	ActiveNodeId string
	ChangedAt    time.Time
	ChangedBy    string
	CaCertPem    string `json:"-"`
	CaKeyPem     string `gorm:"serializer:encstr" json:"-"`

	// informational check of the active exit node, through the active node; never triggers a failover
	ExitCheckAt         *time.Time
	ExitCheckOk         bool
	ExitCheckResult     string // observed egress IP or the probe error
	ExitCheckExpectedIp string // EgressIp of the active exit candidate at check time
}

const HaStateId = 1

// HaJoinToken allows a node to register itself. Only the SHA-256 hash of the token is stored.
type HaJoinToken struct {
	Id        string `gorm:"primaryKey"` // short public id, shown in the UI
	TokenHash string `gorm:"uniqueIndex;size:64" json:"-"`
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsesLeft  int
}

// HaProbePeer links an interface to the peer the orchestrator uses for health probes.
type HaProbePeer struct {
	InterfaceIdentifier InterfaceIdentifier `gorm:"primaryKey"`
	PeerIdentifier      PeerIdentifier      // node health probe, never routed through the exit
	ExitPeerIdentifier  PeerIdentifier      // exit check probe, routed like a normal peer
}

// ExpectedEgressIp returns the IP a probe through the given node must observe. The node probe skips the
// exit node: an exit host only has a tunnel to the active node, so probes through standbys would never
// pass and failover would deadlock.
func (s HaState) ExpectedEgressIp(node HaNode) string {
	return node.PublicIp
}

// HaPeerSession is a connected peer as seen by the kernel of one HA node (active or standby). Each node
// replaces its own rows every statistics tick, so stale rows mean the node stopped reporting.
type HaPeerSession struct {
	NodeId           string         `gorm:"primaryKey"`
	PeerIdentifier   PeerIdentifier `gorm:"primaryKey"`
	LastHandshake    *time.Time
	Endpoint         string
	BytesReceived    uint64
	BytesTransmitted uint64
	UpdatedAt        time.Time
}

// Fresh reports whether the node refreshed the row within 3 collection intervals; rows of dead nodes are not.
func (s HaPeerSession) Fresh(collectionInterval time.Duration) bool {
	return time.Since(s.UpdatedAt) <= 3*collectionInterval
}

// PeerStatus converts the session, IsConnected follows the handshake.
func (s HaPeerSession) PeerStatus(rekeyTimeout time.Duration) PeerStatus {
	ps := PeerStatus{PeerId: s.PeerIdentifier, UpdatedAt: s.UpdatedAt, LastHandshake: s.LastHandshake,
		Endpoint: s.Endpoint, BytesReceived: s.BytesReceived, BytesTransmitted: s.BytesTransmitted}
	ps.CalcConnected(rekeyTimeout)
	return ps
}
