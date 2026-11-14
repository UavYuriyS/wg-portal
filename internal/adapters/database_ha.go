package adapters

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/h44z/wg-portal/internal/domain"
)

// region ha

func (r *SqlRepo) GetHaState(ctx context.Context) (*domain.HaState, error) {
	var s domain.HaState
	err := r.db.WithContext(ctx).Where("id = ?", domain.HaStateId).
		Attrs(domain.HaState{Id: domain.HaStateId}).FirstOrInit(&s).Error
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *SqlRepo) SaveHaState(ctx context.Context, updateFunc func(s *domain.HaState) error) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var s domain.HaState
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", domain.HaStateId).
			Attrs(domain.HaState{Id: domain.HaStateId}).FirstOrInit(&s).Error
		if err != nil {
			return err
		}
		if err := updateFunc(&s); err != nil {
			return err
		}
		return tx.Save(&s).Error
	})
}

func (r *SqlRepo) GetHaNodes(ctx context.Context) ([]domain.HaNode, error) {
	var nodes []domain.HaNode
	err := r.db.WithContext(ctx).Order("priority desc, node_id").Find(&nodes).Error
	return nodes, err
}

func (r *SqlRepo) GetHaNode(ctx context.Context, id string) (*domain.HaNode, error) {
	var n domain.HaNode
	err := r.db.WithContext(ctx).First(&n, "node_id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrNotFound
	}
	return &n, err
}

func (r *SqlRepo) SaveHaNode(ctx context.Context, node *domain.HaNode) error {
	return r.db.WithContext(ctx).Save(node).Error
}

// TouchHaNodeHeartbeat only updates the heartbeat column, so it never overwrites probe results
// written concurrently by the orchestrator on another Galera member.
func (r *SqlRepo) TouchHaNodeHeartbeat(ctx context.Context, id string, at time.Time) error {
	res := r.db.WithContext(ctx).Model(&domain.HaNode{}).Where("node_id = ?", id).
		Update("last_heartbeat", at)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *SqlRepo) UpdateHaNodeProbe(ctx context.Context, id string, ok bool, result string, at time.Time) error {
	fails := gorm.Expr("consecutive_fails + 1")
	if ok {
		fails = gorm.Expr("0")
	}
	return r.db.WithContext(ctx).Model(&domain.HaNode{}).Where("node_id = ?", id).Updates(map[string]any{
		"last_probe_at":     at,
		"last_probe_ok":     ok,
		"last_probe_result": result,
		"consecutive_fails": fails,
	}).Error
}

func (r *SqlRepo) DeleteHaNode(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Delete(&domain.HaNode{}, "node_id = ?", id).Error
}

func (r *SqlRepo) GetHaJoinTokens(ctx context.Context) ([]domain.HaJoinToken, error) {
	var tokens []domain.HaJoinToken
	err := r.db.WithContext(ctx).Order("created_at desc").Find(&tokens).Error
	return tokens, err
}

func (r *SqlRepo) SaveHaJoinToken(ctx context.Context, token *domain.HaJoinToken) error {
	return r.db.WithContext(ctx).Save(token).Error
}

func (r *SqlRepo) DeleteHaJoinToken(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Delete(&domain.HaJoinToken{}, "id = ?", id).Error
}

// ConsumeHaJoinToken atomically decrements the uses of a valid token. It returns domain.ErrNotFound for
// unknown, expired or used up tokens.
func (r *SqlRepo) ConsumeHaJoinToken(ctx context.Context, tokenHash string, now time.Time) error {
	res := r.db.WithContext(ctx).Model(&domain.HaJoinToken{}).
		Where("token_hash = ? AND expires_at > ? AND uses_left > 0", tokenHash, now).
		Update("uses_left", gorm.Expr("uses_left - 1"))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *SqlRepo) GetHaProbePeers(ctx context.Context) ([]domain.HaProbePeer, error) {
	var probes []domain.HaProbePeer
	err := r.db.WithContext(ctx).Find(&probes).Error
	return probes, err
}

func (r *SqlRepo) SaveHaProbePeer(ctx context.Context, probe *domain.HaProbePeer) error {
	return r.db.WithContext(ctx).Save(probe).Error
}

// endregion ha
