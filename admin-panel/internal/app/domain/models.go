// Package domain describes the security console independently of HTTP and storage.
package domain

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

// ROI is a rectangle in normalized video coordinates, not CSS pixels.
type ROI struct {
	Enabled bool    `json:"enabled"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Width   float64 `json:"width"`
	Height  float64 `json:"height"`
}

// Settings is the desired policy. An agent must fetch and acknowledge Revision
// before the console can claim that the policy is applied on the board.
type Settings struct {
	Revision       uint64  `json:"revision"`
	CameraName     string  `json:"camera_name"`
	Source         string  `json:"source"`
	PreviewEnabled bool    `json:"preview_enabled"`
	Armed          bool    `json:"armed"`
	Confidence     float64 `json:"confidence"`
	DwellSeconds   int     `json:"dwell_seconds"`
	Cooldown       int     `json:"cooldown_seconds"`
	ROI            ROI     `json:"roi"`
}

func DefaultSettings() Settings {
	return Settings{
		Revision: 1, CameraName: "Камера 01 · Luckfox", Source: "rtsp://95.163.220.75:8554/mystream",
		PreviewEnabled: true, Confidence: .5, DwellSeconds: 30, Cooldown: 60,
		ROI: ROI{Enabled: false, X: .15, Y: .12, Width: .7, Height: .76},
	}
}

type ValidationError struct{ Field, Message string }

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

func invalid(field, message string) error { return &ValidationError{field, message} }

func (s Settings) Validate() error {
	if strings.TrimSpace(s.CameraName) == "" || len(s.CameraName) > 120 {
		return invalid("camera_name", "укажите название длиной до 120 байт")
	}
	u, err := url.Parse(s.Source)
	if err != nil || u == nil || (u.Scheme != "rtsp" && u.Scheme != "rtsps") || u.Hostname() == "" || len(s.Source) > 2048 || strings.ContainsAny(s.Source, "\r\n") {
		return invalid("source", "нужен адрес rtsp:// или rtsps:// с именем узла")
	}
	if math.IsNaN(s.Confidence) || math.IsInf(s.Confidence, 0) || s.Confidence < .05 || s.Confidence > .99 {
		return invalid("confidence", "допустимы значения от 0.05 до 0.99")
	}
	if s.DwellSeconds < 1 || s.DwellSeconds > 3600 || s.Cooldown < 1 || s.Cooldown > 86400 {
		return invalid("timers", "время в зоне: 1–3600 с; пауза уведомлений: 1–86400 с")
	}
	r := s.ROI
	for _, v := range []float64{r.X, r.Y, r.Width, r.Height} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return invalid("roi", "координаты должны лежать в пределах 0–1")
		}
	}
	if r.Width < .01 || r.Height < .01 || r.X+r.Width > 1.000001 || r.Y+r.Height > 1.000001 {
		return invalid("roi", "зона должна иметь ненулевой размер и помещаться в кадр")
	}
	return nil
}

// PublicSource removes credentials, query parameters and fragment from UI data.
func PublicSource(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "RTSP"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

type Event struct {
	ID         uint64    `json:"id"`
	Time       time.Time `json:"time"`
	Level      string    `json:"level"`
	Type       string    `json:"type"`
	Source     string    `json:"source"`
	Message    string    `json:"message"`
	TrackID    *int      `json:"track_id,omitempty"`
	Confidence *float64  `json:"confidence,omitempty"`
}

func (e Event) Validate() error {
	if e.Level != "info" && e.Level != "warning" && e.Level != "error" {
		return invalid("level", "допустимы info, warning, error")
	}
	if strings.TrimSpace(e.Type) == "" || len(e.Type) > 80 || strings.TrimSpace(e.Message) == "" || len(e.Message) > 2000 || len(e.Source) > 80 {
		return invalid("event", "проверьте тип, источник и текст события")
	}
	if e.Confidence != nil && (math.IsNaN(*e.Confidence) || *e.Confidence < 0 || *e.Confidence > 1) {
		return invalid("confidence", "значение должно быть в пределах 0–1")
	}
	if e.TrackID != nil && *e.TrackID < 0 {
		return invalid("track_id", "ID не может быть отрицательным")
	}
	return nil
}

type EventFilter struct {
	Level, Query, Source string
	Limit                int
	After                uint64
}

// Telemetry comes only from an external detector; no synthetic detections exist.
type Telemetry struct {
	Model          string    `json:"model"`
	FPS            float64   `json:"fps"`
	InferenceMS    float64   `json:"inference_ms"`
	Persons        int       `json:"persons"`
	Vehicles       int       `json:"vehicles"`
	Armed          bool      `json:"armed"`
	PolicyRevision uint64    `json:"policy_revision"`
	ReceivedAt     time.Time `json:"received_at"`
}

func (t Telemetry) Validate() error {
	if len(t.Model) > 120 || math.IsNaN(t.FPS) || math.IsInf(t.FPS, 0) || t.FPS < 0 || t.FPS > 1000 || math.IsNaN(t.InferenceMS) || math.IsInf(t.InferenceMS, 0) || t.InferenceMS < 0 || t.InferenceMS > 60000 || t.Persons < 0 || t.Persons > 10000 || t.Vehicles < 0 || t.Vehicles > 10000 {
		return invalid("telemetry", "некорректные показатели детектора")
	}
	return nil
}

type StreamStatus struct {
	Backend          string    `json:"backend"`
	Available        bool      `json:"available"`
	Online           bool      `json:"online"`
	Registered       bool      `json:"registered"`
	HasProducer      bool      `json:"has_producer"`
	Bytes            uint64    `json:"bytes"`
	BitrateKbps      *float64  `json:"bitrate_kbps"`
	Codecs           []string  `json:"codecs"`
	Error            string    `json:"error,omitempty"`
	CheckedAt        time.Time `json:"checked_at"`
	State            string    `json:"state"`
	Viewers          int       `json:"viewers"`
	LastPacketAt     time.Time `json:"last_packet_at"`
	FrameCount       uint64    `json:"frame_count"`
	InputFPS         *float64  `json:"input_fps"`
	LastFrameAt      time.Time `json:"last_frame_at"`
	RTPPackets       uint64    `json:"rtp_packets"`
	RTPMissing       uint64    `json:"rtp_missing"`
	RTPParseErrors   uint64    `json:"rtp_parse_errors"`
	IncompleteFrames uint64    `json:"incomplete_frame_events"`
	QueueResets      uint64    `json:"queue_resets"`
}

type Status struct {
	StartedAt      time.Time    `json:"started_at"`
	ServerTime     time.Time    `json:"server_time"`
	CameraName     string       `json:"camera_name"`
	Source         string       `json:"source"`
	PreviewEnabled bool         `json:"preview_enabled"`
	Armed          bool         `json:"armed"`
	PolicyRevision uint64       `json:"policy_revision"`
	AgentOnline    bool         `json:"agent_online"`
	PolicyApplied  bool         `json:"policy_applied"`
	AgentEnabled   bool         `json:"agent_enabled"`
	Telemetry      *Telemetry   `json:"telemetry"`
	Stream         StreamStatus `json:"stream"`
	JournalSize    int          `json:"journal_size"`
}

func (f EventFilter) Match(e Event) bool {
	return (f.Level == "" || f.Level == e.Level) && (f.Source == "" || f.Source == e.Source) &&
		(f.After == 0 || e.ID > f.After) && (f.Query == "" || strings.Contains(strings.ToLower(fmt.Sprint(e.Message, " ", e.Type, " ", e.TrackID)), strings.ToLower(f.Query)))
}
