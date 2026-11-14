package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExpectedEgressIpIsAlwaysNodeIp(t *testing.T) {
	node := HaNode{NodeId: "a", PublicIp: "203.0.113.1"}
	assert.Equal(t, "203.0.113.1", HaState{}.ExpectedEgressIp(node))
	// an active exit never changes what the node probe expects
	assert.Equal(t, "203.0.113.1", HaState{ExitCheckExpectedIp: "198.51.100.7"}.ExpectedEgressIp(node))
}

func TestDefaultRouteHelpers(t *testing.T) {
	assert.True(t, HasDefaultRoute("10.0.0.0/24, 0.0.0.0/0"))
	assert.True(t, HasDefaultRoute("::/0"))
	assert.False(t, HasDefaultRoute("10.0.0.0/24"))
	assert.False(t, HasDefaultRoute(""))

	cidrs, _ := CidrsFromString("10.0.0.1/32,0.0.0.0/0,::/0,fd00::/64")
	assert.Equal(t, "10.0.0.1/32,fd00::/64", CidrsToString(StripDefaultRoutes(cidrs)))

	assert.True(t, IsExitRoutingTable(ExitRoutingTable(3)))
	assert.False(t, IsExitRoutingTable(20003))
}
