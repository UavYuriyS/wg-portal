package wgcontroller

import (
	"context"

	"github.com/h44z/wg-portal/internal/config"
	"github.com/h44z/wg-portal/internal/domain"
)

// DbOnlyController is used by the HA orchestrator: interface and peer changes are only written to the
// (replicated) database, the HA nodes apply them to their kernels.
type DbOnlyController struct{}

func NewDbOnlyController() *DbOnlyController { return &DbOnlyController{} }

func (c *DbOnlyController) GetId() domain.InterfaceBackend { return config.LocalBackendName }

func (c *DbOnlyController) GetInterfaces(_ context.Context) ([]domain.PhysicalInterface, error) {
	return nil, nil
}

func (c *DbOnlyController) GetInterface(_ context.Context, id domain.InterfaceIdentifier) (
	*domain.PhysicalInterface,
	error,
) {
	return &domain.PhysicalInterface{Identifier: id}, nil // pretend it exists so no "missing interface" logic runs
}

func (c *DbOnlyController) GetPeers(_ context.Context, _ domain.InterfaceIdentifier) ([]domain.PhysicalPeer, error) {
	return nil, nil
}

func (c *DbOnlyController) SaveInterface(
	_ context.Context,
	id domain.InterfaceIdentifier,
	updateFunc func(pi *domain.PhysicalInterface) (*domain.PhysicalInterface, error),
) error {
	_, err := updateFunc(&domain.PhysicalInterface{Identifier: id})
	return err
}

func (c *DbOnlyController) DeleteInterface(_ context.Context, _ domain.InterfaceIdentifier) error {
	return nil
}

func (c *DbOnlyController) SavePeer(
	_ context.Context,
	_ domain.InterfaceIdentifier,
	id domain.PeerIdentifier,
	updateFunc func(pp *domain.PhysicalPeer) (*domain.PhysicalPeer, error),
) error {
	_, err := updateFunc(&domain.PhysicalPeer{Identifier: id})
	return err
}

func (c *DbOnlyController) DeletePeer(_ context.Context, _ domain.InterfaceIdentifier, _ domain.PeerIdentifier) error {
	return nil
}

func (c *DbOnlyController) PingAddresses(_ context.Context, _ string) (*domain.PingerResult, error) {
	return nil, nil
}
