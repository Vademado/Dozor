package repository

import (
	"os"
	"path/filepath"
	"testing"

	"guard.local/admin/internal/app/domain"
)

func TestSettingsSurviveRestartAndRejectInvalidWrite(t *testing.T) {
	dir := t.TempDir()
	repo := NewSettingsFile(dir)
	cfg, err := repo.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.CameraName = "Двор"
	cfg.Revision = 7
	if err = repo.Save(cfg); err != nil {
		t.Fatal(err)
	}
	restarted := NewSettingsFile(dir)
	got, err := restarted.Load()
	if err != nil || got != cfg {
		t.Fatalf("restart: %+v %v", got, err)
	}
	cfg.Source = "file:///etc/passwd"
	if repo.Save(cfg) == nil {
		t.Fatal("invalid configuration accepted")
	}
	got, err = restarted.Load()
	if err != nil || got.CameraName != "Двор" || got.Revision != 7 {
		t.Fatal("previous config was lost")
	}
}

func TestJournalPersistsFiltersAndIgnoresPartialTail(t *testing.T) {
	dir := t.TempDir()
	journal, err := NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := journal.Append(domain.Event{Level: "info", Source: "system", Type: "startup", Message: "Старт"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := journal.Append(domain.Event{Level: "warning", Source: "detector", Type: "DWELL_TIMEOUT", Message: "Человек в зоне"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID <= first.ID {
		t.Fatal("IDs not increasing")
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"id":`)
	_ = f.Close()
	restarted, err := NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	items := restarted.List(domain.EventFilter{Level: "warning", Query: "человек", After: first.ID})
	if len(items) != 1 || items[0].ID != second.ID {
		t.Fatalf("unexpected filtered journal: %+v", items)
	}
	third, err := restarted.Append(domain.Event{Level: "info", Source: "system", Type: "restart", Message: "Новый запуск"})
	if err != nil {
		t.Fatal(err)
	}
	if third.ID != second.ID+1 {
		t.Fatal("ID sequence not recovered")
	}
	again, err := NewJournal(dir)
	if err != nil || again.Size() != 3 {
		t.Fatalf("valid event after partial tail was lost: %v", err)
	}
}
