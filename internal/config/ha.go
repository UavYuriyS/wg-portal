package config

import (
	"fmt"
	"time"
)

type HaRole string

const (
	HaRoleStandalone   HaRole = "standalone"   // classic single instance, the default
	HaRoleOrchestrator HaRole = "orchestrator" // UI, control plane and failover; never touches WireGuard
	HaRoleNode         HaRole = "node"         // WireGuard server; applies the replicated DB to the kernel
)

// HaConfig configures the high availability setup, see docs/design/ha.md.
type HaConfig struct {
	Role HaRole `yaml:"role"`
	// NodeId identifies this party in the cluster. Defaults to the hostname.
	NodeId string `yaml:"node_id"`
	// PublicIp is the address users (node) or Galera peers (all roles) reach this party on.
	PublicIp string `yaml:"public_ip"`
	// Priority decides which healthy standby gets promoted first (higher wins).
	Priority int `yaml:"priority"`
	// DataDir holds the cluster CA, TLS certificates and generated Galera/wg-portal configs.
	DataDir string `yaml:"data_dir"`

	HeartbeatInterval   time.Duration `yaml:"heartbeat_interval"`
	ReconcileInterval   time.Duration `yaml:"reconcile_interval"`
	ProbeInterval       time.Duration `yaml:"probe_interval"`
	ProbeTimeout        time.Duration `yaml:"probe_timeout"`
	ProbeUrl            string        `yaml:"probe_url"` // returns the caller IP as plain text (ipify)
	FailThreshold       int           `yaml:"fail_threshold"`
	HeartbeatTimeout    time.Duration `yaml:"heartbeat_timeout"`
	MinFailoverInterval time.Duration `yaml:"min_failover_interval"`
	AutoFailover        bool          `yaml:"auto_failover"`
	OnPromote           string        `yaml:"on_promote"` // shell command, gets NODE_ID and NODE_IP env vars

	// Node only: used by "wg-portal -ha-init" to join the cluster.
	OrchestratorUrl string `yaml:"orchestrator_url"`
	JoinToken       string `yaml:"join_token"`

	// Orchestrator only: credentials handed to joining nodes.
	Galera HaGaleraConfig `yaml:"galera"`
	Dns    HaDnsConfig    `yaml:"dns"`
}

type HaGaleraConfig struct {
	ClusterName string `yaml:"cluster_name"`
	SstUser     string `yaml:"sst_user"`
	SstPassword string `yaml:"sst_password"`
}

type HaDnsConfig struct {
	RegRu HaRegRuConfig `yaml:"regru"`
}

type HaRegRuConfig struct {
	ApiUrl    string `yaml:"api_url"`
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
	Zone      string `yaml:"zone"`      // e.g. example.com
	Subdomain string `yaml:"subdomain"` // e.g. vpn
}

func (h HaConfig) Enabled() bool        { return h.Role == HaRoleOrchestrator || h.Role == HaRoleNode }
func (h HaConfig) IsNode() bool         { return h.Role == HaRoleNode }
func (h HaConfig) IsOrchestrator() bool { return h.Role == HaRoleOrchestrator }

func (h HaConfig) Validate() error {
	switch h.Role {
	case "", HaRoleStandalone:
		return nil
	case HaRoleOrchestrator, HaRoleNode:
	default:
		return fmt.Errorf("invalid ha.role %q", h.Role)
	}
	if h.NodeId == "" {
		return fmt.Errorf("ha.node_id must be set")
	}
	if h.Role == HaRoleOrchestrator && h.PublicIp == "" {
		return fmt.Errorf("ha.public_ip must be set on the orchestrator (Galera address of this party)")
	}
	if h.Role == HaRoleOrchestrator && h.Dns.RegRu.Zone != "" && h.Dns.RegRu.Subdomain == "" {
		return fmt.Errorf("ha.dns.regru.subdomain must be set")
	}
	return nil
}
