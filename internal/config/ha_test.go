package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The orchestrator reads the statistics the active node replicates, so they must stay enabled for the API.
func TestApplyHaRoleOverrides_OrchestratorKeepsStatistics(t *testing.T) {
	cfg := &Config{Ha: HaConfig{Role: HaRoleOrchestrator}}
	cfg.Statistics.CollectPeerData = true
	cfg.Statistics.CollectInterfaceData = true

	applyHaRoleOverrides(cfg)

	require.True(t, cfg.Statistics.CollectPeerData)
	require.True(t, cfg.Statistics.CollectInterfaceData)
	require.False(t, cfg.Core.ImportExisting)
}
