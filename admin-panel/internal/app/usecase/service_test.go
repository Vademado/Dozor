package usecase

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"guard.local/admin/internal/app/domain"
	"guard.local/admin/internal/app/repository"
)

type fakeBridge struct {
	online, registered bool
	source             string
}

func (f *fakeBridge) Start(ctx context.Context, source string) error {
	f.registered = true
	f.source = source
	return nil
}
func (f *fakeBridge) Stop(context.Context) error { f.registered = false; return nil }
func (f *fakeBridge) Status(context.Context) (domain.StreamStatus, error) {
	s := domain.StreamStatus{Online: f.online, Registered: f.registered, CheckedAt: time.Now(), Codecs: []string{}}
	if !f.online {
		s.Error = "offline"
		return s, errors.New("offline")
	}
	return s, nil
}

func newService(t *testing.T, bridge *fakeBridge) *Service {
	t.Helper()
	dir := t.TempDir()
	journal, err := repository.NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(repository.NewSettingsFile(dir), journal, bridge, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPreviewRecoversBridgeAndDoesNotArmSystem(t *testing.T) {
	bridge := &fakeBridge{}
	s := newService(t, bridge)
	s.Refresh(context.Background())
	if s.Status().Stream.Online {
		t.Fatal("offline bridge marked online")
	}
	bridge.online = true
	s.Refresh(context.Background())
	if !bridge.registered || bridge.source != s.Settings().Source {
		t.Fatal("stream not restored")
	}
	_, err := s.Preview(context.Background(), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bridge.registered || s.Settings().Armed || s.Status().PreviewEnabled {
		t.Fatal("stop changed wrong state")
	}
	_, err = s.Preview(context.Background(), true, nil)
	if err != nil || !bridge.registered {
		t.Fatal("restart failed")
	}
}

func TestPolicyRequiresAgentAcknowledgementAndRejectsStaleSave(t *testing.T) {
	s := newService(t, &fakeBridge{online: true})
	old := s.Settings()
	cfg, err := s.Arm(true)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Status().Armed || s.Status().PolicyApplied {
		t.Fatal("unacknowledged policy claimed applied")
	}
	if _, err = s.Update(old); !errors.Is(err, ErrConflict) {
		t.Fatal("stale policy overwrite accepted")
	}
	if err = s.Telemetry(domain.Telemetry{Armed: true, PolicyRevision: cfg.Revision - 1}); err != nil {
		t.Fatal(err)
	}
	if s.Status().PolicyApplied {
		t.Fatal("wrong revision acknowledged")
	}
	if err = s.Telemetry(domain.Telemetry{Armed: true, PolicyRevision: cfg.Revision}); err != nil {
		t.Fatal(err)
	}
	if !s.Status().PolicyApplied || !s.Status().AgentOnline {
		t.Fatal("valid acknowledgement not recognized")
	}
	s.mu.Lock()
	s.telemetry.ReceivedAt = time.Now().Add(-20 * time.Second)
	s.mu.Unlock()
	if s.Status().PolicyApplied || s.Status().AgentOnline {
		t.Fatal("stale telemetry reported live")
	}
}
