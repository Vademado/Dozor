// Package usecase coordinates policy, streaming, telemetry and the audit journal.
// Its interfaces are owned here, so neither HTTP nor filesystem details leak in.
package usecase

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"guard.local/admin/internal/app/domain"
)

var ErrConflict = errors.New("настройки уже изменились; обновите страницу и повторите действие")

type SettingsRepository interface {
	Load() (domain.Settings, error)
	Save(domain.Settings) error
}

type EventRepository interface {
	Append(domain.Event) (domain.Event, error)
	List(domain.EventFilter) []domain.Event
	Size() int
}

type StreamGateway interface {
	Start(context.Context, string) error
	Stop(context.Context) error
	Status(context.Context) (domain.StreamStatus, error)
}

type Service struct {
	opMu             sync.Mutex // serializes policy changes and upstream stream registration
	playerMu         sync.Mutex
	playerLogs       map[string]time.Time
	mu               sync.RWMutex // protects short-lived state snapshots, never held during I/O
	settings         domain.Settings
	streamStatus     domain.StreamStatus
	telemetry        *domain.Telemetry
	registeredSource string
	started          time.Time
	agentEnabled     bool
	settingsRepo     SettingsRepository
	events           EventRepository
	stream           StreamGateway
	log              *slog.Logger
}

func New(settings SettingsRepository, events EventRepository, stream StreamGateway, agentEnabled bool, log *slog.Logger) (*Service, error) {
	cfg, err := settings.Load()
	if err != nil {
		return nil, err
	}
	if err = cfg.Validate(); err != nil {
		return nil, err
	}
	if err = settings.Save(cfg); err != nil {
		return nil, err
	}
	s := &Service{settings: cfg, started: time.Now().UTC(), agentEnabled: agentEnabled, settingsRepo: settings, events: events, stream: stream, log: log}
	s.audit("info", "server_started", "system", "Панель управления запущена")
	return s, nil
}

func (s *Service) Settings() domain.Settings { s.mu.RLock(); defer s.mu.RUnlock(); return s.settings }

// ConsoleEvent records lifecycle only. Commands, terminal output and SSH
// credentials are deliberately absent from the application journal.
func (s *Service) ConsoleEvent(kind string) {
	messages := map[string]string{
		"console_connected":      "Оператор открыл SSH-консоль камеры",
		"console_disconnected":   "SSH-консоль камеры закрыта",
		"console_connect_failed": "Не удалось открыть SSH-консоль камеры",
	}
	if message, ok := messages[kind]; ok {
		level := "info"
		if kind == "console_connect_failed" {
			level = "warning"
		}
		s.audit(level, kind, "operator", message)
	}
}

// Update uses optimistic concurrency: two browser tabs cannot silently overwrite policy.
func (s *Service) Update(cfg domain.Settings) (domain.Settings, error) {
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.saveLocked(cfg, "settings_updated", "Настройки системы сохранены")
}

func (s *Service) saveLocked(cfg domain.Settings, kind, message string) (domain.Settings, error) {
	previous := s.Settings()
	if cfg.Revision != previous.Revision {
		return previous, ErrConflict
	}
	cfg.Revision = previous.Revision + 1
	if err := s.settingsRepo.Save(cfg); err != nil {
		return previous, err
	}
	s.mu.Lock()
	s.settings = cfg
	s.mu.Unlock()
	if cfg.Source != previous.Source || cfg.PreviewEnabled != previous.PreviewEnabled {
		s.registeredSource = ""
	}
	s.audit("info", kind, "operator", message)
	return cfg, nil
}

func (s *Service) Preview(ctx context.Context, enabled bool, source *string) (domain.Settings, error) {
	s.opMu.Lock()
	cfg := s.Settings()
	cfg.PreviewEnabled = enabled
	if source != nil {
		cfg.Source = *source
	}
	if err := cfg.Validate(); err != nil {
		s.opMu.Unlock()
		return cfg, err
	}
	message := "Просмотр остановлен; режим охраны не изменён"
	if enabled {
		message = "Запрошен запуск просмотра камеры"
	}
	cfg, err := s.saveLocked(cfg, "preview_changed", message)
	s.opMu.Unlock()
	if err == nil {
		s.Refresh(ctx)
	}
	return cfg, err
}

func (s *Service) Arm(armed bool) (domain.Settings, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	cfg := s.Settings()
	cfg.Armed = armed
	message := "Запрошено снятие с охраны; ожидается подтверждение агента"
	if armed {
		message = "Запрошена постановка на охрану; ожидается подтверждение агента"
	}
	return s.saveLocked(cfg, "policy_changed", message)
}

// Monitor synchronizes panel preview policy and reads streaming status.
func (s *Service) Monitor(ctx context.Context) {
	s.Refresh(ctx)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Refresh(ctx)
		}
	}
}

func (s *Service) Refresh(ctx context.Context) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	cfg := s.Settings()
	next, err := s.stream.Status(ctx)
	if err == nil {
		if cfg.PreviewEnabled && (!next.Registered || s.registeredSource != cfg.Source) {
			if err = s.stream.Start(ctx, cfg.Source); err != nil {
				next.Error = err.Error()
			} else {
				s.registeredSource = cfg.Source
				next, _ = s.stream.Status(ctx)
			}
		} else if !cfg.PreviewEnabled && next.Registered {
			if err = s.stream.Stop(ctx); err != nil {
				next.Error = err.Error()
			} else {
				next, _ = s.stream.Status(ctx)
			}
		}
	}
	s.mu.Lock()
	previous := s.streamStatus
	if previous.HasProducer && next.HasProducer && next.Bytes >= previous.Bytes && previous.CheckedAt.Before(next.CheckedAt) && !previous.CheckedAt.IsZero() {
		kbps := float64(next.Bytes-previous.Bytes) * 8 / 1000 / next.CheckedAt.Sub(previous.CheckedAt).Seconds()
		next.BitrateKbps = &kbps
		if next.Backend != "go2rtc" && next.FrameCount >= previous.FrameCount {
			fps := float64(next.FrameCount-previous.FrameCount) / next.CheckedAt.Sub(previous.CheckedAt).Seconds()
			next.InputFPS = &fps
		}
	}
	s.streamStatus = next
	s.mu.Unlock()
	if previous.CheckedAt.IsZero() || previous.Online != next.Online || previous.Error != next.Error {
		if next.Error != "" {
			s.audit("warning", "rtsp_unavailable", "stream", next.Error)
		} else if next.Online {
			s.audit("info", "rtsp_connected", "stream", "Прямое RTSP-подключение к камере установлено")
		}
	}
}

func (s *Service) Status() domain.Status {
	s.mu.RLock()
	cfg, stream := s.settings, s.streamStatus
	var telemetry *domain.Telemetry
	if s.telemetry != nil {
		copy := *s.telemetry
		telemetry = &copy
	}
	s.mu.RUnlock()
	online := telemetry != nil && time.Since(telemetry.ReceivedAt) < 15*time.Second
	applied := online && telemetry.PolicyRevision == cfg.Revision && telemetry.Armed == cfg.Armed
	return domain.Status{
		StartedAt: s.started, ServerTime: time.Now().UTC(), CameraName: cfg.CameraName,
		Source: domain.PublicSource(cfg.Source), PreviewEnabled: cfg.PreviewEnabled, Armed: cfg.Armed,
		PolicyRevision: cfg.Revision, AgentOnline: online, PolicyApplied: applied, AgentEnabled: s.agentEnabled,
		Telemetry: telemetry, Stream: stream, JournalSize: s.events.Size(),
	}
}

func (s *Service) Telemetry(t domain.Telemetry) error {
	if err := t.Validate(); err != nil {
		return err
	}
	t.ReceivedAt = time.Now().UTC()
	s.mu.Lock()
	wasOffline := s.telemetry == nil || time.Since(s.telemetry.ReceivedAt) >= 15*time.Second
	s.telemetry = &t
	s.mu.Unlock()
	if wasOffline {
		s.audit("info", "agent_connected", "detector", "Получена телеметрия агента детекции")
	}
	return nil
}

func (s *Service) Ingest(event domain.Event) (domain.Event, error) {
	event.Source = "detector"
	if err := event.Validate(); err != nil {
		return event, err
	}
	return s.events.Append(event)
}

func (s *Service) PlayerEvent(kind string) error {
	allowed := map[string]string{
		"player_stalled":     "Просмотр: новые кадры не поступают, выполняется переподключение",
		"player_error":       "Просмотр: браузер сообщил об ошибке воспроизведения",
		"player_reconnected": "Просмотр: оператор запросил переподключение",
		"snapshot_created":   "Оператор сохранил снимок текущего кадра на компьютер",
		"rtsp_timeout":       "Плата не отвечает на RTSP-порту: прямое подключение завершилось тайм-аутом",
		"rtsp_refused":       "RTSP-сервер платы отклонил соединение",
		"rtsp_unauthorized":  "RTSP-сервер платы отклонил авторизацию",
		"codec_unsupported":  "Браузер не поддерживает кодек или профиль камеры в WebRTC",
	}
	message, ok := allowed[kind]
	if !ok {
		return &domain.ValidationError{Field: "type", Message: "неизвестное событие плеера"}
	}
	s.playerMu.Lock()
	if s.playerLogs == nil {
		s.playerLogs = make(map[string]time.Time)
	}
	if last := s.playerLogs[kind]; !last.IsZero() && time.Since(last) < 30*time.Second {
		s.playerMu.Unlock()
		return nil
	}
	s.playerLogs[kind] = time.Now()
	s.playerMu.Unlock()
	level := "info"
	if kind == "player_stalled" || kind == "player_error" || kind == "rtsp_timeout" || kind == "rtsp_refused" || kind == "rtsp_unauthorized" || kind == "codec_unsupported" {
		level = "warning"
	}
	s.audit(level, kind, "browser", message)
	return nil
}

func (s *Service) Events(filter domain.EventFilter) []domain.Event { return s.events.List(filter) }

func (s *Service) audit(level, kind, source, message string) {
	_, err := s.events.Append(domain.Event{Level: level, Type: kind, Source: source, Message: message})
	if err != nil {
		s.log.Error("journal write failed", "error", err)
	}
}
