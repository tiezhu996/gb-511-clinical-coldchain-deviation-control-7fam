package repository

import (
	"context"
	"errors"

	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/dto"
	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// errReleaseGateBlocked aborts the release transaction after the gate check
// collected the blocking excursion/disposition codes.
var errReleaseGateBlocked = errors.New("release gate blocked")

// TransportContainerRepository owns all persistence operations for 运输容器.
type TransportContainerRepository interface {
	List(context.Context, dto.PageQuery) (Page[model.TransportContainer], error)
	Get(context.Context, uint) (model.TransportContainer, error)
	Create(context.Context, *model.TransportContainer) error
	Update(context.Context, uint, uint, *model.TransportContainer, ...*model.AuditLog) error
	Delete(context.Context, uint) error
	CountByStatus(context.Context) (map[string]int64, error)
	ReleaseWithGateCheck(context.Context, uint, uint, *model.TransportContainer, *model.AuditLog) ([]string, error)
}

type transportContainerRepository struct {
	store *Store[model.TransportContainer]
}

func NewTransportContainerRepository(db *gorm.DB) TransportContainerRepository {
	return &transportContainerRepository{store: NewStore[model.TransportContainer](db)}
}

func (r *transportContainerRepository) List(ctx context.Context, q dto.PageQuery) (Page[model.TransportContainer], error) {
	return r.store.List(ctx, q)
}
func (r *transportContainerRepository) Get(ctx context.Context, id uint) (model.TransportContainer, error) {
	return r.store.Get(ctx, id)
}
func (r *transportContainerRepository) Create(ctx context.Context, item *model.TransportContainer) error {
	return r.store.Create(ctx, item)
}
func (r *transportContainerRepository) Update(ctx context.Context, id, version uint, item *model.TransportContainer, audits ...*model.AuditLog) error {
	return r.store.Update(ctx, id, version, item, audits...)
}
func (r *transportContainerRepository) Delete(ctx context.Context, id uint) error {
	return r.store.Delete(ctx, id)
}
func (r *transportContainerRepository) CountByStatus(ctx context.Context) (map[string]int64, error) {
	return r.store.CountByStatus(ctx)
}

// ReleaseWithGateCheck runs the 质量放行 gate and the status migration in one
// transaction: the container row is locked, every excursion on the container must
// be closed and every final disposition must be a release, then the optimistic
// update and the audit entry are written together. Blocking excursion or
// disposition codes are returned so the caller can list them; any failure rolls
// back both the container state and the audit entry.
func (r *transportContainerRepository) ReleaseWithGateCheck(ctx context.Context, id, expectedVersion uint, item *model.TransportContainer, audit *model.AuditLog) ([]string, error) {
	blockers := make([]string, 0)
	err := r.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current model.TransportContainer
		if err := lockContainerForUpdate(tx, "id = ?", id).First(&current).Error; err != nil {
			return err
		}
		excursions := make([]model.ExcursionEvent, 0)
		if err := tx.Where("container_code = ?", current.Code).Find(&excursions).Error; err != nil {
			return err
		}
		excursionCodes := make([]string, 0, len(excursions))
		for _, excursion := range excursions {
			excursionCodes = append(excursionCodes, excursion.Code)
			if excursion.Status != "closed" {
				blockers = append(blockers, excursion.Code)
			}
		}
		if len(excursionCodes) > 0 {
			decisions := make([]model.DispositionDecision, 0)
			if err := tx.Where("excursion_code IN ? AND status IN ?", excursionCodes, []string{"quarantine", "discard"}).Find(&decisions).Error; err != nil {
				return err
			}
			for _, decision := range decisions {
				blockers = append(blockers, decision.Code)
			}
		}
		if len(blockers) > 0 {
			return errReleaseGateBlocked
		}
		result := tx.Model(&model.TransportContainer{}).Where("id = ? AND version = ?", id, expectedVersion).
			Select("*").Omit("id", "code", "created_at", "deleted_at").Updates(item)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrVersionConflict
		}
		return tx.Create(audit).Error
	})
	if errors.Is(err, errReleaseGateBlocked) {
		return blockers, nil
	}
	return nil, err
}

// lockContainerForUpdate reads one container row FOR UPDATE so a concurrent
// excursion registration and a quality release serialize on the same row. SQLite
// has no row locking clause; its single-writer lock already serializes the
// competing transaction, so the clause is skipped there.
func lockContainerForUpdate(tx *gorm.DB, where string, value any) *gorm.DB {
	query := tx.Model(&model.TransportContainer{}).Where(where, value)
	if tx.Dialector.Name() != "sqlite" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return query
}
