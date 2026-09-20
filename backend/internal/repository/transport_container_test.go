package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func openGateTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory database: %v", err)
	}
	if err := db.AutoMigrate(&model.TransportContainer{}, &model.ExcursionEvent{}, &model.DispositionDecision{}, &model.AuditLog{}); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	return db
}

func seedGateContainer(t *testing.T, db *gorm.DB, code, status string) model.TransportContainer {
	t.Helper()
	item := model.TransportContainer{
		BaseModel: model.BaseModel{Code: code, Name: "放行门禁测试容器", Status: status, Version: 1},
		Facility:  "测试中心", Owner: "质量组",
	}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("seed container: %v", err)
	}
	return item
}

func seedGateExcursion(t *testing.T, db *gorm.DB, code, containerCode, status string) {
	t.Helper()
	item := model.ExcursionEvent{
		BaseModel:     model.BaseModel{Code: code, Name: "门禁测试偏差", Status: status, Version: 1},
		ContainerCode: containerCode, WindowCode: "TW-GATE", DurationMinutes: 5,
	}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("seed excursion: %v", err)
	}
}

func seedGateDecision(t *testing.T, db *gorm.DB, code, excursionCode, status string) {
	t.Helper()
	item := model.DispositionDecision{
		BaseModel:     model.BaseModel{Code: code, Name: "门禁测试处置", Status: status, Version: 1},
		ExcursionCode: excursionCode,
	}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("seed decision: %v", err)
	}
}

func releaseAudit(containerID uint) *model.AuditLog {
	return &model.AuditLog{Actor: "reviewer", RequestID: "req-gate", Action: "transition", EntityType: "TransportContainer", EntityID: containerID, BeforeState: "quarantine", AfterState: "cleared", Detail: "gate test"}
}

func releasedItem(container model.TransportContainer) *model.TransportContainer {
	container.Status = "cleared"
	container.Version = 2
	container.UpdatedAt = time.Now().UTC()
	return &container
}

func countRows(t *testing.T, db *gorm.DB, table string) int64 {
	t.Helper()
	var total int64
	if err := db.Table(table).Count(&total).Error; err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return total
}

func TestReleaseWithGateCheckBlocksOpenExcursion(t *testing.T) {
	db := openGateTestDB(t)
	container := seedGateContainer(t, db, "TC-GATE-1", "quarantine")
	seedGateExcursion(t, db, "EE-OPEN", "TC-GATE-1", "open")
	repo := NewTransportContainerRepository(db)
	blockers, err := repo.ReleaseWithGateCheck(context.Background(), container.ID, 1, releasedItem(container), releaseAudit(container.ID))
	if err != nil {
		t.Fatalf("gate check should return blockers, not error: %v", err)
	}
	if len(blockers) != 1 || blockers[0] != "EE-OPEN" {
		t.Fatalf("expected blocker EE-OPEN, got %v", blockers)
	}
	var stored model.TransportContainer
	if err := db.First(&stored, container.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "quarantine" || stored.Version != 1 {
		t.Fatalf("blocked release must not change container state, got status=%s version=%d", stored.Status, stored.Version)
	}
	if total := countRows(t, db, "audit_logs"); total != 0 {
		t.Fatalf("blocked release must not write audit entries, got %d", total)
	}
}

func TestReleaseWithGateCheckBlocksNonReleaseDisposition(t *testing.T) {
	db := openGateTestDB(t)
	container := seedGateContainer(t, db, "TC-GATE-2", "quarantine")
	seedGateExcursion(t, db, "EE-CLOSED", "TC-GATE-2", "closed")
	seedGateDecision(t, db, "DD-DISCARD", "EE-CLOSED", "discard")
	repo := NewTransportContainerRepository(db)
	blockers, err := repo.ReleaseWithGateCheck(context.Background(), container.ID, 1, releasedItem(container), releaseAudit(container.ID))
	if err != nil {
		t.Fatalf("gate check should return blockers, not error: %v", err)
	}
	if len(blockers) != 1 || blockers[0] != "DD-DISCARD" {
		t.Fatalf("expected blocker DD-DISCARD, got %v", blockers)
	}
	var stored model.TransportContainer
	if err := db.First(&stored, container.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "quarantine" {
		t.Fatalf("blocked release must keep quarantine, got %s", stored.Status)
	}
}

func TestReleaseWithGateCheckAllowsClosedReleasedExcursions(t *testing.T) {
	db := openGateTestDB(t)
	container := seedGateContainer(t, db, "TC-GATE-3", "quarantine")
	seedGateExcursion(t, db, "EE-DONE", "TC-GATE-3", "closed")
	seedGateDecision(t, db, "DD-RELEASE", "EE-DONE", "release")
	repo := NewTransportContainerRepository(db)
	blockers, err := repo.ReleaseWithGateCheck(context.Background(), container.ID, 1, releasedItem(container), releaseAudit(container.ID))
	if err != nil {
		t.Fatalf("release should succeed: %v", err)
	}
	if len(blockers) != 0 {
		t.Fatalf("expected no blockers, got %v", blockers)
	}
	var stored model.TransportContainer
	if err := db.First(&stored, container.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "cleared" || stored.Version != 2 {
		t.Fatalf("expected cleared version 2, got status=%s version=%d", stored.Status, stored.Version)
	}
	if total := countRows(t, db, "audit_logs"); total != 1 {
		t.Fatalf("release must write exactly one audit entry, got %d", total)
	}
}

func TestCreateForContainerRejectsReleasedContainer(t *testing.T) {
	db := openGateTestDB(t)
	seedGateContainer(t, db, "TC-GATE-4", "cleared")
	repo := NewExcursionEventRepository(db)
	item := model.ExcursionEvent{
		BaseModel:     model.BaseModel{Code: "EE-LATE", Name: "放行后偏差", Status: "open", Version: 1},
		ContainerCode: "TC-GATE-4", WindowCode: "TW-GATE", DurationMinutes: 3,
	}
	err := repo.CreateForContainer(context.Background(), &item, &model.AuditLog{Actor: "operator", Action: "create", EntityType: "ExcursionEvent"})
	if !errors.Is(err, ErrContainerReleased) {
		t.Fatalf("expected ErrContainerReleased, got %v", err)
	}
	if total := countRows(t, db, "excursion_events"); total != 0 {
		t.Fatalf("rejected excursion must be rolled back, got %d rows", total)
	}
	if total := countRows(t, db, "audit_logs"); total != 0 {
		t.Fatalf("rejected excursion must not write audit entries, got %d", total)
	}
}

func TestCreateForContainerAcceptsQuarantinedContainer(t *testing.T) {
	db := openGateTestDB(t)
	seedGateContainer(t, db, "TC-GATE-5", "quarantine")
	repo := NewExcursionEventRepository(db)
	item := model.ExcursionEvent{
		BaseModel:     model.BaseModel{Code: "EE-OK", Name: "隔离中偏差", Status: "open", Version: 1},
		ContainerCode: "TC-GATE-5", WindowCode: "TW-GATE", DurationMinutes: 7,
	}
	audit := &model.AuditLog{Actor: "operator", Action: "create", EntityType: "ExcursionEvent"}
	if err := repo.CreateForContainer(context.Background(), &item, audit); err != nil {
		t.Fatalf("create on quarantined container should succeed: %v", err)
	}
	if item.ID == 0 || audit.EntityID != item.ID {
		t.Fatalf("audit entry must reference the created excursion, got item=%d audit=%d", item.ID, audit.EntityID)
	}
	if total := countRows(t, db, "audit_logs"); total != 1 {
		t.Fatalf("create must write exactly one audit entry, got %d", total)
	}
}
