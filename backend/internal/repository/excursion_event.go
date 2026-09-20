package repository

import (
	"context"
	"errors"

	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/dto"
	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrContainerReleased is returned when a new deviation targets a container
// that has already been质量放行; the release and the deviation cannot both win.
var ErrContainerReleased = errors.New("container already released; new deviations are rejected")

// ExcursionEventRepository owns all persistence operations for 偏差事件.
type ExcursionEventRepository interface {
	List(context.Context, dto.PageQuery) (Page[model.ExcursionEvent], error)
	Get(context.Context, uint) (model.ExcursionEvent, error)
	Create(context.Context, *model.ExcursionEvent) error
	CreateWithContainerGate(context.Context, *model.ExcursionEvent) error
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

// CreateWithContainerGate inserts the deviation while holding a lock on the
// linked container row. If the container has already been released the insert
// is rejected, so a concurrent release and deviation registration serialize:
// whichever transaction commits first decides the outcome of the other.
func (r *excursionEventRepository) CreateWithContainerGate(ctx context.Context, item *model.ExcursionEvent) error {
	return r.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if item.ContainerCode != "" {
			lock := tx
			if supportsRowLocking(tx.Dialector.Name()) {
				lock = lock.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			var container model.TransportContainer
			err := lock.Where("code = ?", item.ContainerCode).First(&container).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err == nil && container.Status == "cleared" {
				return ErrContainerReleased
			}
		}
		return tx.Create(item).Error
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
