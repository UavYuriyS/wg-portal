package adapters

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/h44z/wg-portal/internal/domain"
)

// region routing

// GetExitCandidates returns the exit candidates of an interface, or of all interfaces if id is empty. Rows of
// deleted peers are skipped: a key rotation deletes the peer before RenameExitCandidatePeer re-keys its row.
func (r *SqlRepo) GetExitCandidates(ctx context.Context, id domain.InterfaceIdentifier) (
	[]domain.ExitCandidate,
	error,
) {
	var candidates []domain.ExitCandidate
	q := r.db.WithContext(ctx).Model(&domain.ExitCandidate{}).Select("exit_candidates.*").
		Joins("JOIN peers ON peers.identifier = exit_candidates.peer_identifier AND " +
			"peers.interface_identifier = exit_candidates.interface_identifier").
		Order("exit_candidates.created_at, exit_candidates.peer_identifier")
	if id != "" {
		q = q.Where("exit_candidates.interface_identifier = ?", id)
	}
	err := q.Find(&candidates).Error
	return candidates, err
}

// RenameExitCandidatePeer moves the candidate rows of a peer to its new identifier (public key rotation).
func (r *SqlRepo) RenameExitCandidatePeer(ctx context.Context, oldId, newId domain.PeerIdentifier) error {
	return r.db.WithContext(ctx).Model(&domain.ExitCandidate{}).Where("peer_identifier = ?", oldId).
		Update("peer_identifier", newId).Error
}

// ReplaceExitCandidates replaces all exit candidates of an interface in one transaction.
func (r *SqlRepo) ReplaceExitCandidates(
	ctx context.Context,
	id domain.InterfaceIdentifier,
	candidates []domain.ExitCandidate,
) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("interface_identifier = ?", id).Delete(&domain.ExitCandidate{}).Error; err != nil {
			return err
		}
		if len(candidates) == 0 {
			return nil
		}
		return tx.Create(&candidates).Error
	})
}

// GetRoutingLists returns all lists ordered by name; Content is only loaded if withContent is set.
func (r *SqlRepo) GetRoutingLists(ctx context.Context, withContent bool) ([]domain.RoutingList, error) {
	var lists []domain.RoutingList
	q := r.db.WithContext(ctx).Order("name, id")
	if !withContent {
		q = q.Omit("content")
	}
	err := q.Find(&lists).Error
	return lists, err
}

func (r *SqlRepo) GetRoutingList(ctx context.Context, id string) (*domain.RoutingList, error) {
	var list domain.RoutingList
	err := r.db.WithContext(ctx).First(&list, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &list, nil
}

func (r *SqlRepo) SaveRoutingList(ctx context.Context, list *domain.RoutingList) error {
	return r.db.WithContext(ctx).Save(list).Error
}

func (r *SqlRepo) DeleteRoutingList(ctx context.Context, id string) error {
	res := r.db.WithContext(ctx).Delete(&domain.RoutingList{}, "id = ?", id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *SqlRepo) GetRoutingSettings(ctx context.Context) (*domain.RoutingSettings, error) {
	var s domain.RoutingSettings
	err := r.db.WithContext(ctx).Where("id = ?", domain.RoutingSettingsId).
		Attrs(domain.RoutingSettings{Id: domain.RoutingSettingsId}).FirstOrInit(&s).Error
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *SqlRepo) SaveRoutingSettings(ctx context.Context, s *domain.RoutingSettings) error {
	s.Id = domain.RoutingSettingsId
	return r.db.WithContext(ctx).Save(s).Error
}

// endregion routing
