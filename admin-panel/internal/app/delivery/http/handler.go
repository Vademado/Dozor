// Package http is the delivery layer: JSON, middleware, SSE and static assets.
package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"guard.local/admin/internal/app/domain"
	"guard.local/admin/internal/app/usecase"
	"guard.local/admin/web"
)

type Handler struct {
	service      *usecase.Service
	log          *slog.Logger
	console      consoleConnector
	consoleHost  string
	consoleSlots chan struct{}
}

func New(service *usecase.Service, log *slog.Logger) *Handler {
	return &Handler{service: service, log: log, consoleHost: "192.168.1.199", consoleSlots: make(chan struct{}, 8)}
}

func (h *Handler) WithConsole(connector consoleConnector) *Handler {
	h.console = connector
	return h
}

// WithConsoleHost configures the board independently of an RTSP relay address.
func (h *Handler) WithConsoleHost(host string) *Handler {
	if host != "" {
		h.consoleHost = host
	}
	return h
}

func (h *Handler) Routes(media http.Handler, password, agentToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, h.service.Settings()) })
	mux.HandleFunc("PUT /api/settings", h.settings)
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, h.service.Status()) })
	mux.HandleFunc("POST /api/preview", h.preview)
	mux.HandleFunc("POST /api/arm", h.arm)
	mux.HandleFunc("POST /api/player/events", h.playerEvent)
	mux.HandleFunc("GET /api/events", h.events)
	mux.HandleFunc("GET /api/events/export", h.export)
	mux.HandleFunc("GET /api/events/stream", h.eventStream)
	mux.HandleFunc("GET /api/console/config", h.consoleConfig)
	mux.HandleFunc("GET /api/console/ws", h.consoleWebSocket)
	// Production embeds go2rtc directly, without installing a media proxy.
	if media == nil {
		media = http.NotFoundHandler()
	}
	mux.Handle("POST /api/stream/offer", media)
	mux.Handle("GET /api/stream/ws", media)
	mux.Handle("DELETE /api/stream/viewers/{id}", media)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" && !isAsset(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		http.FileServerFS(web.Files).ServeHTTP(w, r)
	})
	// The detector has its own bearer token, not the operator's Basic credentials.
	agent := http.NewServeMux()
	agent.HandleFunc("GET /api/agent/policy", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, h.service.Settings()) })
	agent.HandleFunc("POST /api/agent/events", h.ingest)
	agent.HandleFunc("POST /api/agent/telemetry", h.telemetry)
	root := http.NewServeMux()
	root.Handle("/api/agent/", agentAuth(agentToken, agent))
	root.Handle("/", basicAuth(password, mux))
	return h.middleware(root)
}

func isAsset(path string) bool {
	return len(path) > 8 && (path[:8] == "/assets/" || path[:8] == "/vendor/")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func readJSON(w http.ResponseWriter, r *http.Request, body any) error {
	if r.Header.Get("Content-Type") != "application/json" {
		return &domain.ValidationError{Field: "content_type", Message: "нужен Content-Type: application/json"}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(body); err != nil {
		return &domain.ValidationError{Field: "body", Message: "некорректный JSON или превышен размер 16 КиБ"}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return &domain.ValidationError{Field: "body", Message: "ожидается один JSON-объект"}
	}
	return nil
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	var validation *domain.ValidationError
	if errors.As(err, &validation) {
		writeJSON(w, 400, map[string]string{"error": validation.Message, "field": validation.Field})
		return
	}
	if errors.Is(err, usecase.ErrConflict) {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	h.log.Error("request failed", "error", err)
	writeJSON(w, 500, map[string]string{"error": "Не удалось выполнить действие. Проверьте доступ к каталогу данных и журнал сервера."})
}

func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	var cfg domain.Settings
	if err := readJSON(w, r, &cfg); err != nil {
		h.fail(w, err)
		return
	}
	cfg, err := h.service.Update(cfg)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, cfg)
}

func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool   `json:"enabled"`
		Source  *string `json:"source"`
	}
	if err := readJSON(w, r, &body); err != nil {
		h.fail(w, err)
		return
	}
	if body.Enabled == nil {
		h.fail(w, &domain.ValidationError{Field: "enabled", Message: "укажите enabled"})
		return
	}
	cfg, err := h.service.Preview(r.Context(), *body.Enabled, body.Source)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"settings": cfg, "status": h.service.Status()})
}

func (h *Handler) arm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Armed *bool `json:"armed"`
	}
	if err := readJSON(w, r, &body); err != nil {
		h.fail(w, err)
		return
	}
	if body.Armed == nil {
		h.fail(w, &domain.ValidationError{Field: "armed", Message: "укажите armed"})
		return
	}
	cfg, err := h.service.Arm(*body.Armed)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 200, cfg)
}

func (h *Handler) playerEvent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type string `json:"type"`
	}
	if err := readJSON(w, r, &body); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.service.PlayerEvent(body.Type); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func eventFilter(r *http.Request) domain.EventFilter {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	return domain.EventFilter{Level: r.URL.Query().Get("level"), Query: r.URL.Query().Get("q"), Source: r.URL.Query().Get("source"), Limit: limit, After: after}
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"items": h.service.Events(eventFilter(r))})
}

func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	filter := eventFilter(r)
	filter.Limit = 5000
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="guard-events.jsonl"`)
	w.Header().Set("Cache-Control", "no-store")
	encoder := json.NewEncoder(w)
	for _, event := range h.service.Events(filter) {
		if encoder.Encode(event) != nil {
			return
		}
	}
}

// SSE is a resumable notification feed, not the video transport. Each tick also
// carries current status so an agent going offline is reflected without events.
func (h *Handler) eventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	after, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	if after == 0 {
		after, _ = strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	}
	controller := http.NewResponseController(w)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		_ = controller.SetWriteDeadline(time.Now().Add(8 * time.Second))
		data, _ := json.Marshal(h.service.Status())
		if _, err := fmt.Fprintf(w, "event: status\ndata: %s\n\n", data); err != nil {
			return
		}
		if after != 0 {
			events := h.service.Events(domain.EventFilter{After: after, Limit: 5000})
			for i := len(events) - 1; i >= 0; i-- {
				data, _ = json.Marshal(events[i])
				after = events[i].ID
				if _, err := fmt.Fprintf(w, "id: %d\nevent: journal\ndata: %s\n\n", after, data); err != nil {
					return
				}
			}
		} else {
			latest := h.service.Events(domain.EventFilter{Limit: 1})
			if len(latest) > 0 {
				after = latest[0].ID
			}
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *Handler) ingest(w http.ResponseWriter, r *http.Request) {
	var event domain.Event
	if err := readJSON(w, r, &event); err != nil {
		h.fail(w, err)
		return
	}
	event, err := h.service.Ingest(event)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, 201, event)
}

func (h *Handler) telemetry(w http.ResponseWriter, r *http.Request) {
	var telemetry domain.Telemetry
	if err := readJSON(w, r, &telemetry); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.service.Telemetry(telemetry); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(204)
}
