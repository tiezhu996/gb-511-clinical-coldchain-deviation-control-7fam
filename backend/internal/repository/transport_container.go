package repository

import (
	"context"
	"errors"

	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/dto"
	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ReleaseBlocker identifies one 偏差事件 that prevents a container from being
// released: either it is not closed yet, or its final disposition is not 放行.
type ReleaseBlocker struct {
	ExcursionCode    string `json:"excursionCode"`
	Status           string `json:"status"`
	FinalDisposition string `json:"finalDisposition"`
}

// errReleaseGateRolledBack aborts the release transaction after blockers were
// recorded, so a rejected release never changes container state or audit.
var errReleaseGateRolledBack = errors.New("release gate blocked; transaction rolled back")

// TransportContainerRepository owns all persistence operations for 运输容器.
type TransportContainerRepository interface {
	List(context.Context, dto.PageQuery) (Page[model.TransportContainer], error)
	Get(context.Context, uint) (model.TransportContainer, error)
	Create(context.Context, *model.TransportContainer) error
	Update(context.Context, uint, uint, *model.TransportContainer) error
	Delete(context.Context, uint) error
	CountByStatus(context.Context) (map[string]int64, error)
	// ReleaseWithGate verifies every deviation linked to the container inside
	// one transaction and only then applies the status change plus audit.
	// It returns the blocking deviations when the gate rejects the release.
	ReleaseWithGate(context.Context, uint, uint, *model.TransportContainer, *model.AuditLog) ([]ReleaseBlocker, error)
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
func (r *transportContainerRepository) Update(ctx context.Context, id, version uint, item *model.TransportContainer) error {
	return r.store.Update(ctx, id, version, item)
}
func (r *transportContainerRepository) Delete(ctx context.Context, id uint) error {
	return r.store.Delete(ctx, id)
}
func (r *transportContainerRepository) CountByStatus(ctx context.Context) (map[string]int64, error) {
	return r.store.CountByStatus(ctx)
}

// ReleaseWithGate locks the container row, re-checks every linked deviation
// and commits status change plus audit log atomically. Concurrent deviation
// registration takes the same row lock, so a release and a new deviation
// against the same container can never both succeed.
func (r *transportContainerRepository) ReleaseWithGate(ctx context.Context, id, expectedVersion uint, item *model.TransportContainer, audit *model.AuditLog) ([]ReleaseBlocker, error) {
	blockers := make([]ReleaseBlocker, 0)
	err := r.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		lock := tx
		if supportsRowLocking(tx.Dialector.Name()) {
			lock = lock.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var container model.TransportContainer
		if err := lock.First(&container, id).Error; err != nil {
			return err
		}
		found, err := findReleaseBlockers(tx, container.Code)
		if err != nil {
			return err
		}
		if len(found) > 0 {
			blockers = found
			return errReleaseGateRolledBack
		}
		result := tx.Model(&model.TransportContainer{}).Where("id = ? AND version = ?", id, expectedVersion).
			Select("*").Omit("id", "code", "created_at", "deleted_at").Updates(item)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrVersionConflict
		}
		if audit != nil {
			return tx.Create(audit).Error
		}
		return nil
	})
	if errors.Is(err, errReleaseGateRolledBack) {
		return blockers, nil
	}
	return nil, err
}

// findReleaseBlockers lists every deviation of the container that is either
// still open or whose final disposition is 隔离/报废 instead of 放行.
func findReleaseBlockers(tx *gorm.DB, containerCode string) ([]ReleaseBlocker, error) {
	excursions := make([]model.ExcursionEvent, 0)
	if err := tx.Where("container_code = ?", containerCode).Order("code").Find(&excursions).Error; err != nil {
		return nil, err
	}
	blockers := make([]ReleaseBlocker, 0)
	for _, excursion := range excursions {
		if excursion.Status != "closed" {
			blockers = append(blockers, ReleaseBlocker{ExcursionCode: excursion.Code, Status: excursion.Status})
			continue
		}
		final, err := finalDisposition(tx, excursion.Code)
		if err != nil {
			return nil, err
		}
		if final != "release" {
			blockers = append(blockers, ReleaseBlocker{ExcursionCode: excursion.Code, Status: excursion.Status, FinalDisposition: final})
		}
	}
	return blockers, nil
}

// finalDisposition returns the status of the latest final (release/quarantine/
// discard) disposition for a deviation, or "" when none exists.
func finalDisposition(tx *gorm.DB, excursionCode string) (string, error) {
	var decision model.DispositionDecision
	err := tx.Where("excursion_code = ? AND status IN ?", excursionCode, []string{"release", "quarantine", "discard"}).
		Order("id DESC").First(&decision).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return decision.Status, nil
}

// supportsRowLocking reports whether the dialect honours SELECT ... FOR UPDATE;
// SQLite serializes writers on its own, so the clause is skipped there.
func supportsRowLocking(dialect string) bool {
	return dialect == "postgres" || dialect == "mysql"
}
