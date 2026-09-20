package repository

import (
	"context"
	"errors"

	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/dto"
	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/model"
	"gorm.io/gorm"
)

// ErrContainerReleased rejects a new excursion when the parent container has
// already been released by the quality gate.
var ErrContainerReleased = errors.New("container already released; new excursions are rejected")

// ExcursionEventRepository owns all persistence operations for 偏差事件.
type ExcursionEventRepository interface {
	List(context.Context, dto.PageQuery) (Page[model.ExcursionEvent], error)
	Get(context.Context, uint) (model.ExcursionEvent, error)
	Create(context.Context, *model.ExcursionEvent) error
	CreateForContainer(context.Context, *model.ExcursionEvent, *model.AuditLog) error
	Update(context.Context, uint, uint, *model.ExcursionEvent, ...*model.AuditLog) error
	Delete(context.Context, uint) error
	CountByStatus(context.Context) (map[string]int64, error)
}

type excursionEventRepository struct {
	store *Store[model.ExcursionEvent]
}

func NewExcursionEventRepository(db *gorm.DB) ExcursionEventRepository {
	return &excursionEventRepository{store: NewStore[model.ExcursionEvent](db)}
}

func (r *excursionEventRepository) List(ctx context.Context, q dto.PageQuery) (Page[model.ExcursionEvent], error) {
	return r.store.List(ctx, q)
}
func (r *excursionEventRepository) Get(ctx context.Context, id uint) (model.ExcursionEvent, error) {
	return r.store.Get(ctx, id)
}
func (r *excursionEventRepository) Create(ctx context.Context, item *model.ExcursionEvent) error {
	return r.store.Create(ctx, item)
}

// CreateForContainer inserts 偏差事件 and its audit entry in one transaction
// while holding a lock on the parent container row. A released (cleared)
// container rejects new excursions, so a concurrent quality release and an
// excursion registration can never both succeed; the loser rolls back without
// touching the container state or the audit trail.
func (r *excursionEventRepository) CreateForContainer(ctx context.Context, item *model.ExcursionEvent, audit *model.AuditLog) error {
	return r.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var container model.TransportContainer
		if err := lockContainerForUpdate(tx, "code = ?", item.ContainerCode).First(&container).Error; err != nil {
			return err
		}
		if container.Status == "cleared" {
			return ErrContainerReleased
		}
		if err := tx.Create(item).Error; err != nil {
			return err
		}
		audit.EntityID = item.ID
		return tx.Create(audit).Error
	})
}
func (r *excursionEventRepository) Update(ctx context.Context, id, version uint, item *model.ExcursionEvent, audits ...*model.AuditLog) error {
	return r.store.Update(ctx, id, version, item, audits...)
}
func (r *excursionEventRepository) Delete(ctx context.Context, id uint) error {
	return r.store.Delete(ctx, id)
}
func (r *excursionEventRepository) CountByStatus(ctx context.Context) (map[string]int64, error) {
	return r.store.CountByStatus(ctx)
}
