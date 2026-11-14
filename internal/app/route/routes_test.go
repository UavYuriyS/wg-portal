package route

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/h44z/wg-portal/internal/domain"
)

type fakeRoutesController struct {
	domain.InterfaceController
	set, removed []domain.Cidr
}

func (f *fakeRoutesController) SetRoutes(_ context.Context, info domain.RoutingTableInfo) error {
	f.set = info.AllowedIps
	return nil
}

func (f *fakeRoutesController) RemoveRoutes(_ context.Context, info domain.RoutingTableInfo) error {
	f.removed = info.AllowedIps
	return nil
}

type fakeControllerManager struct{ c *fakeRoutesController }

func (f fakeControllerManager) GetController(domain.Interface) domain.InterfaceController { return f.c }

type fakeDb struct{ candidates []domain.ExitCandidate }

func (fakeDb) GetInterface(context.Context, domain.InterfaceIdentifier) (*domain.Interface, error) {
	return nil, domain.ErrNotFound
}

func (f fakeDb) GetExitCandidates(context.Context, domain.InterfaceIdentifier) ([]domain.ExitCandidate, error) {
	return f.candidates, nil
}

func TestDefaultRoutesStrippedInExitMode(t *testing.T) {
	allowed, err := domain.CidrsFromString("10.11.12.2/32,0.0.0.0/0,::/0")
	require.NoError(t, err)
	info := domain.RoutingTableInfo{Interface: domain.Interface{Identifier: "wg0"}, AllowedIps: allowed}

	// without candidates the wg-quick behaviour is unchanged
	rc := &fakeRoutesController{}
	m := Manager{db: fakeDb{}, wgController: fakeControllerManager{rc}}
	require.NoError(t, m.syncRoutes(context.Background(), info))
	assert.Equal(t, allowed, rc.set)

	// exit mode: default routes never reach the kernel route table
	m.db = fakeDb{candidates: []domain.ExitCandidate{{InterfaceIdentifier: "wg0", PeerIdentifier: "exit"}}}
	require.NoError(t, m.syncRoutes(context.Background(), info))
	require.NoError(t, m.removeRoutes(context.Background(), info))
	assert.Equal(t, "10.11.12.2/32", domain.CidrsToString(rc.set))
	assert.Equal(t, "10.11.12.2/32", domain.CidrsToString(rc.removed))
}
