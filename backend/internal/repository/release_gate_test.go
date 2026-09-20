package repository

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("unwrap sql db: %v", err)
	}
	// A single connection serializes concurrent transactions deterministically.
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.TransportContainer{}, &model.ExcursionEvent{}, &model.DispositionDecision{}, &model.AuditLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func seedGateContainer(t *testing.T, db *gorm.DB, code, status string) model.TransportContainer {
	t.Helper()
	container := model.TransportContainer{
		BaseModel: model.BaseModel{Code: code, Name: "门禁测试容器 " + code, Status: status, Version: 1},
		SensorID:  "SN-" + code,
	}
	if err := db.Create(&container).Error; err != nil {
		t.Fatalf("seed container: %v", err)
	}
	return container
}

func seedGateExcursion(t *testing.T, db *gorm.DB, code, containerCode, status string) {
	t.Helper()
	excursion := model.ExcursionEvent{
		BaseModel:     model.BaseModel{Code: code, Name: "偏差 " + code, Status: status, Version: 1},
		ContainerCode: containerCode,
	}
	if err := db.Create(&excursion).Error; err != nil {
		t.Fatalf("seed excursion: %v", err)
	}
}

func seedGateDisposition(t *testing.T, db *gorm.DB, code, excursionCode, status string) {
	t.Helper()
	decision := model.DispositionDecision{
		BaseModel:     model.BaseModel{Code: code, Name: "处置 " + code, Status: status, Version: 1},
		ExcursionCode: excursionCode,
	}
	if err := db.Create(&decision).Error; err != nil {
		t.Fatalf("seed disposition: %v", err)
	}
}

func releaseCandidate(container model.TransportContainer) *model.TransportContainer {
	container.Status = "cleared"
	container.Version = container.Version + 1
	container.UpdatedAt = time.Now().UTC()
	return &container
}

func gateAudit(containerID uint) *model.AuditLog {
	return &model.AuditLog{Actor: "reviewer", RequestID: "req-gate", Action: "transition", EntityType: "TransportContainer", EntityID: containerID, BeforeState: "quarantine", AfterState: "cleared", Detail: "gate test", CreatedAt: time.Now().UTC()}
}

func containerState(t *testing.T, db *gorm.DB, id uint) (string, uint) {
	t.Helper()
	var stored model.TransportContainer
	if err := db.First(&stored, id).Error; err != nil {
		t.Fatalf("reload container: %v", err)
	}
	return stored.Status, stored.Version
}

func auditCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var total int64
	if err := db.Model(&model.AuditLog{}).Count(&total).Error; err != nil {
		t.Fatalf("count audits: %v", err)
	}
	return total
}

func TestReleaseWithGateBlocksUnclosedExcursion(t *testing.T) {
	db := openGateTestDB(t)
	container := seedGateContainer(t, db, "TC-G1", "quarantine")
	seedGateExcursion(t, db, "EE-G1", "TC-G1", "open")
	seedGateExcursion(t, db, "EE-G2", "TC-G1", "in_review")

	repo := NewTransportContainerRepository(db)
	blockers, err := repo.ReleaseWithGate(context.Background(), container.ID, container.Version, releaseCandidate(container), gateAudit(container.ID))
	if err != nil {
		t.Fatalf("gate returned error: %v", err)
	}
	if len(blockers) != 2 || blockers[0].ExcursionCode != "EE-G1" || blockers[1].ExcursionCode != "EE-G2" {
		t.Fatalf("expected both unclosed deviations as blockers, got %+v", blockers)
	}
	status, version := containerState(t, db, container.ID)
	if status != "quarantine" || version != container.Version {
		t.Fatalf("rejected release must not change container, got status=%s version=%d", status, version)
	}
	if total := auditCount(t, db); total != 0 {
		t.Fatalf("rejected release must not write audit, got %d rows", total)
	}
}

func TestReleaseWithGateBlocksNonReleaseDisposition(t *testing.T) {
	db := openGateTestDB(t)
	container := seedGateContainer(t, db, "TC-G2", "quarantine")
	seedGateExcursion(t, db, "EE-G3", "TC-G2", "closed")
	seedGateDisposition(t, db, "DD-G3", "EE-G3", "discard")
	seedGateExcursion(t, db, "EE-G4", "TC-G2", "closed")
	seedGateDisposition(t, db, "DD-G4", "EE-G4", "quarantine")

	repo := NewTransportContainerRepository(db)
	blockers, err := repo.ReleaseWithGate(context.Background(), container.ID, container.Version, releaseCandidate(container), gateAudit(container.ID))
	if err != nil {
		t.Fatalf("gate returned error: %v", err)
	}
	if len(blockers) != 2 || blockers[0].FinalDisposition != "discard" || blockers[1].FinalDisposition != "quarantine" {
		t.Fatalf("expected discard/quarantine blockers, got %+v", blockers)
	}
	status, _ := containerState(t, db, container.ID)
	if status != "quarantine" {
		t.Fatalf("container must stay quarantined, got %s", status)
	}
}

func TestReleaseWithGateAllowsReleasedDeviations(t *testing.T) {
	db := openGateTestDB(t)
	container := seedGateContainer(t, db, "TC-G3", "quarantine")
	seedGateExcursion(t, db, "EE-G5", "TC-G3", "closed")
	seedGateDisposition(t, db, "DD-G5", "EE-G5", "release")

	repo := NewTransportContainerRepository(db)
	blockers, err := repo.ReleaseWithGate(context.Background(), container.ID, container.Version, releaseCandidate(container), gateAudit(container.ID))
	if err != nil {
		t.Fatalf("gate returned error: %v", err)
	}
	if len(blockers) != 0 {
		t.Fatalf("expected no blockers, got %+v", blockers)
	}
	status, version := containerState(t, db, container.ID)
	if status != "cleared" || version != container.Version+1 {
		t.Fatalf("expected cleared container with bumped version, got status=%s version=%d", status, version)
	}
	if total := auditCount(t, db); total != 1 {
		t.Fatalf("successful release must write exactly one audit row, got %d", total)
	}
}

func TestCreateWithContainerGateRejectsReleasedContainer(t *testing.T) {
	db := openGateTestDB(t)
	seedGateContainer(t, db, "TC-G4", "cleared")
	seedGateContainer(t, db, "TC-G5", "quarantine")
	repo := NewExcursionEventRepository(db)

	rejected := model.ExcursionEvent{BaseModel: model.BaseModel{Code: "EE-G6", Name: "放行后偏差", Status: "open", Version: 1}, ContainerCode: "TC-G4"}
	if err := repo.CreateWithContainerGate(context.Background(), &rejected); !errors.Is(err, ErrContainerReleased) {
		t.Fatalf("expected ErrContainerReleased, got %v", err)
	}
	accepted := model.ExcursionEvent{BaseModel: model.BaseModel{Code: "EE-G7", Name: "隔离中偏差", Status: "open", Version: 1}, ContainerCode: "TC-G5"}
	if err := repo.CreateWithContainerGate(context.Background(), &accepted); err != nil {
		t.Fatalf("expected insert on quarantined container to succeed, got %v", err)
	}
}

func TestReleaseGateAndDeviationCreateAreMutuallyExclusive(t *testing.T) {
	for attempt := 0; attempt < 5; attempt++ {
		t.Run(fmt.Sprintf("attempt_%d", attempt), func(t *testing.T) {
			db := openGateTestDB(t)
			container := seedGateContainer(t, db, "TC-G6", "quarantine")
			containers := NewTransportContainerRepository(db)
			excursions := NewExcursionEventRepository(db)

			start := make(chan struct{})
			var wg sync.WaitGroup
			var releaseErr error
			var blockers []ReleaseBlocker
			var createErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				blockers, releaseErr = containers.ReleaseWithGate(context.Background(), container.ID, container.Version, releaseCandidate(container), gateAudit(container.ID))
			}()
			go func() {
				defer wg.Done()
				<-start
				deviation := model.ExcursionEvent{BaseModel: model.BaseModel{Code: "EE-G8", Name: "并发偏差", Status: "open", Version: 1}, ContainerCode: "TC-G6"}
				createErr = excursions.CreateWithContainerGate(context.Background(), &deviation)
			}()
			close(start)
			wg.Wait()

			releaseWon := releaseErr == nil && len(blockers) == 0
			createWon := createErr == nil
			if releaseWon == createWon {
				t.Fatalf("exactly one of release/create must win, releaseWon=%v createWon=%v (blockers=%+v, createErr=%v)", releaseWon, createWon, blockers, createErr)
			}
			if createWon {
				if len(blockers) != 1 || blockers[0].ExcursionCode != "EE-G8" {
					t.Fatalf("release must report the concurrent deviation as blocker, got %+v", blockers)
				}
				status, _ := containerState(t, db, container.ID)
				if status != "quarantine" {
					t.Fatalf("container must stay quarantined, got %s", status)
				}
			}
			if releaseWon && !errors.Is(createErr, ErrContainerReleased) {
				t.Fatalf("losing deviation create must fail with ErrContainerReleased, got %v", createErr)
			}
			if total := auditCount(t, db); (releaseWon && total != 1) || (!releaseWon && total != 0) {
				t.Fatalf("audit rows inconsistent with winner, got %d", total)
			}
		})
	}
}
