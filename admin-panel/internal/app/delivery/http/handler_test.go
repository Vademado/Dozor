package http

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"guard.local/admin/internal/app/domain"
	"guard.local/admin/internal/app/repository"
	"guard.local/admin/internal/app/usecase"
)

type offlineBridge struct{}

func (offlineBridge) Start(context.Context, string) error { return nil }
func (offlineBridge) Stop(context.Context) error          { return nil }
func (offlineBridge) Status(context.Context) (domain.StreamStatus, error) {
	return domain.StreamStatus{}, nil
}

func testHandler(t *testing.T, password, token string) http.Handler {
	t.Helper()
	dir := t.TempDir()
	journal, err := repository.NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := usecase.New(repository.NewSettingsFile(dir), journal, offlineBridge{}, token != "", log)
	if err != nil {
		t.Fatal(err)
	}
	return New(s, log).Routes(nil, password, token)
}

func TestAuthenticationCSRFAndStrictJSON(t *testing.T) {
	h := testHandler(t, "a-long-admin-password", "a-long-agent-token-for-testing")
	cases := []struct {
		name, method, path, body, origin, auth string
		want                                   int
	}{
		{"operator auth required", "GET", "/api/settings", "", "", "", 401},
		{"cross origin rejected", "POST", "/api/arm", `{"armed":true}`, "http://evil.invalid", "basic", 403},
		{"unknown fields rejected", "POST", "/api/arm", `{"armed":true,"command":"reboot"}`, "", "basic", 400},
		{"multiple documents rejected", "POST", "/api/arm", `{"armed":true} {}`, "", "basic", 400},
		{"explicit boolean required", "POST", "/api/arm", `{}`, "", "basic", 400},
		{"operator may change desired policy", "POST", "/api/arm", `{"armed":true}`, "", "basic", 200},
		{"agent needs separate token", "GET", "/api/agent/policy", "", "", "basic", 401},
		{"agent policy access", "GET", "/api/agent/policy", "", "", "bearer", 200},
		{"native SDP route disabled", "POST", "/api/stream/offer", "{}", "", "basic", 404},
		{"media proxy disabled", "GET", "/api/stream/ws", "", "", "basic", 404},
		{"unlisted media API blocked", "GET", "/api/stream/config", "", "", "basic", 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://localhost"+tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.auth == "basic" {
				r.SetBasicAuth("admin", "a-long-admin-password")
			}
			if tc.auth == "bearer" {
				r.Header.Set("Authorization", "Bearer a-long-agent-token-for-testing")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestLocalConsoleRejectsRebindingHostAndServesEmbeddedAssets(t *testing.T) {
	h := testHandler(t, "", "")
	for _, path := range []string{"/", "/assets/app.js", "/assets/player.js"} {
		r := httptest.NewRequest("GET", "http://localhost"+path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("asset %s: %d", path, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "http://attacker.invalid/api/settings", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("untrusted Host accepted without authentication")
	}
	r = httptest.NewRequest("GET", "http://localhost/api/agent/policy", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal("disabled agent endpoint exposed")
	}
}

func TestSSEPublishesStatusAndHonorsCancellation(t *testing.T) {
	h := testHandler(t, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "http://localhost/api/events/stream", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "event: status") || !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatal("SSE status missing")
	}
}
