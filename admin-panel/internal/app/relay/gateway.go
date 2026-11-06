// Package relay connects the console to the local go2rtc media gateway.
// The console never repacketizes or decodes video in this mode.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"guard.local/admin/internal/app/domain"
)

const StreamName = "luckfox"

type Gateway struct {
	mu             sync.Mutex
	base           string
	client         *http.Client
	source         string
	registered     bool
	producer       string
	lastRaw, total uint64
	lastPacket     time.Time
}

func New(base string) *Gateway {
	return &Gateway{base: strings.TrimRight(base, "/"), client: &http.Client{Timeout: 3 * time.Second}}
}

func (g *Gateway) request(ctx context.Context, path string, query url.Values) ([]byte, int, error) {
	endpoint := g.base + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, errors.New("неверный адрес go2rtc")
	}
	response, err := g.client.Do(req)
	if err != nil {
		return nil, 0, errors.New("go2rtc недоступен. Проверьте адрес API и состояние сервиса.")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	return data, response.StatusCode, err
}

// Start enables observation for the panel. It never changes the existing
// luckfox stream in go2rtc or opens another RTSP connection to the camera.
func (g *Gateway) Start(ctx context.Context, source string) error {
	u, err := url.Parse(source)
	if err != nil || u.Host == "" || (u.Scheme != "rtsp" && u.Scheme != "rtsps") {
		return errors.New("нужен RTSP/RTSPS-адрес камеры")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.registered && g.source == source {
		return nil
	}
	g.source = source
	g.registered = true
	g.producer = ""
	g.lastRaw = 0
	g.total = 0
	g.lastPacket = time.Time{}
	return nil
}

// Stop only disables the panel's observation. Other go2rtc viewers keep running.
func (g *Gateway) Stop(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.registered = false
	g.source = ""
	g.producer = ""
	g.lastRaw = 0
	return nil
}

type streamInfo struct {
	Producers []struct {
		ID         json.Number `json:"id"`
		RemoteAddr string      `json:"remote_addr"`
		Receivers  []struct {
			Codec struct {
				Name string `json:"codec_name"`
				Type string `json:"codec_type"`
			} `json:"codec"`
			Bytes   uint64 `json:"bytes"`
			Packets uint64 `json:"packets"`
		} `json:"receivers"`
	} `json:"producers"`
	Consumers []json.RawMessage `json:"consumers"`
}

func (g *Gateway) Status(ctx context.Context) (domain.StreamStatus, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := domain.StreamStatus{Backend: "go2rtc", Registered: g.registered, Codecs: []string{}, CheckedAt: time.Now().UTC(), State: "stopped"}
	data, status, err := g.request(ctx, "/api/streams", url.Values{"src": {StreamName}})
	if err != nil {
		s.Error = err.Error()
		s.State = "unavailable"
		return s, err
	}
	if status != 200 && status != 404 {
		s.Error = "go2rtc отклонил запрос состояния"
		return s, errors.New(s.Error)
	}
	s.Available = true
	if status == 404 {
		s.Registered = false
		s.Error = "В go2rtc не найден поток luckfox. Проверьте его go2rtc.yaml."
		return s, errors.New(s.Error)
	}
	if !g.registered {
		return s, nil
	}
	var info streamInfo
	if err = json.Unmarshal(data, &info); err != nil {
		s.Error = "Некорректный ответ go2rtc"
		return s, errors.New(s.Error)
	}
	s.State = "ready"
	s.Viewers = len(info.Consumers)
	var raw uint64
	var id string
	for _, producer := range info.Producers {
		for _, receiver := range producer.Receivers {
			if receiver.Codec.Type != "video" {
				continue
			}
			name := strings.ToUpper(receiver.Codec.Name)
			if name == "H264" {
				name = "H.264"
			}
			if name == "H265" || name == "HEVC" {
				name = "H.265"
			}
			s.Codecs = append(s.Codecs, name)
			raw += receiver.Bytes
			s.RTPPackets += receiver.Packets
			id += producer.ID.String() + ":"
			if producer.RemoteAddr != "" {
				s.Online = true
				s.State = "streaming"
			}
		}
	}
	if id != "" {
		increment := raw
		if id == g.producer && raw >= g.lastRaw {
			increment = raw - g.lastRaw
		}
		g.total += increment
		if increment > 0 {
			g.lastPacket = s.CheckedAt
		}
		g.producer = id
		g.lastRaw = raw
	} else {
		g.producer = ""
		g.lastRaw = 0
	}
	s.Bytes = g.total
	s.LastPacketAt = g.lastPacket
	s.HasProducer = s.Online && !g.lastPacket.IsZero() && time.Since(g.lastPacket) < 10*time.Second
	return s, nil
}

// Close leaves the independently started go2rtc process and its streams running.
func (g *Gateway) Close() {}
