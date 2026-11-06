package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"guard.local/admin/internal/app/console"
)

type consoleConnector interface {
	Connect(context.Context, console.ConnectRequest) (console.Remote, error)
}

func (h *Handler) consoleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"enabled": h.console != nil, "host": h.consoleHost, "port": 22, "user": "root"})
}

type consoleMessage struct {
	Type  string `json:"type"`
	Cols  int    `json:"cols,omitempty"`
	Rows  int    `json:"rows,omitempty"`
	Bytes int    `json:"bytes,omitempty"`
}

type inboundConsole struct {
	message consoleMessage
	input   []byte
}

// The upgrade is protected by the same operator auth and origin checks as the
// rest of the panel. Credentials are accepted only in the first WS frame.
func (h *Handler) consoleWebSocket(w http.ResponseWriter, r *http.Request) {
	if h.console == nil {
		writeJSON(w, 503, map[string]string{"error": "Консоль камеры не настроена"})
		return
	}
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Host != r.Host || (origin.Scheme != "http" && origin.Scheme != "https") || r.URL.RawQuery != "" {
		writeJSON(w, 403, map[string]string{"error": "Откройте консоль из интерфейса панели"})
		return
	}
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(peer)
	if r.TLS == nil && (ip == nil || !ip.IsLoopback()) {
		writeJSON(w, 403, map[string]string{"error": "Для SSH-консоли используйте localhost или HTTPS"})
		return
	}
	select {
	case h.consoleSlots <- struct{}{}:
		defer func() { <-h.consoleSlots }()
	default:
		writeJSON(w, 429, map[string]string{"error": "Открыто слишком много SSH-сеансов. Закройте неиспользуемые консоли"})
		return
	}
	upgrader := websocket.Upgrader{HandshakeTimeout: 5 * time.Second, ReadBufferSize: 4096, WriteBufferSize: 8192, CheckOrigin: func(*http.Request) bool { return true }}
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stopSocket := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopSocket()
	connection.SetReadLimit(16384)
	_ = connection.SetReadDeadline(time.Now().Add(15 * time.Second))
	kind, first, err := connection.ReadMessage()
	if err != nil || kind != websocket.TextMessage {
		return
	}
	var hello struct {
		Type string `json:"type"`
		console.ConnectRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(first))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&hello)
	if err == nil {
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			err = errors.New("extra data")
		}
	}
	first = nil
	decoder = nil
	writer := newConsoleWriter(ctx, connection)
	if err != nil || hello.Type != "connect" {
		_ = writer.json(map[string]string{"type": "error", "message": "Некорректный запрос подключения"})
		return
	}
	if err = hello.Validate(); err != nil {
		_ = writer.json(map[string]string{"type": "error", "message": err.Error()})
		return
	}
	_ = connection.SetReadDeadline(time.Now().Add(90 * time.Second))
	connection.SetPongHandler(func(string) error { return connection.SetReadDeadline(time.Now().Add(90 * time.Second)) })
	incoming := make(chan inboundConsole, 8)
	go func() {
		defer cancel()
		for {
			kind, payload, readErr := connection.ReadMessage()
			if readErr != nil {
				return
			}
			message := inboundConsole{}
			if kind == websocket.BinaryMessage {
				message.input = payload
			} else if kind != websocket.TextMessage || json.Unmarshal(payload, &message.message) != nil {
				return
			}
			select {
			case incoming <- message:
			case <-ctx.Done():
				return
			}
		}
	}()
	remote, err := h.console.Connect(ctx, hello.ConnectRequest)
	hello.Password = ""
	if err != nil {
		var hostKey *console.HostKeyError
		if errors.As(err, &hostKey) {
			_ = writer.json(map[string]any{"type": "host_key", "message": hostKey.Error(), "fingerprint": hostKey.Fingerprint, "algorithm": hostKey.Algorithm, "changed": hostKey.Changed})
		} else {
			_ = writer.json(map[string]string{"type": "error", "message": err.Error()})
			h.service.ConsoleEvent("console_connect_failed")
		}
		return
	}
	defer remote.Close()
	stopRemote := context.AfterFunc(ctx, func() { _ = remote.Close() })
	defer stopRemote()
	h.service.ConsoleEvent("console_connected")
	defer h.service.ConsoleEvent("console_disconnected")
	if err = writer.json(map[string]string{"type": "connected", "message": "SSH-соединение установлено"}); err != nil {
		return
	}
	var output sync.WaitGroup
	output.Add(2)
	for _, reader := range []io.Reader{remote.Output(), remote.Errors()} {
		go func(reader io.Reader) {
			defer output.Done()
			if _, err := io.Copy(writer, reader); err != nil {
				cancel()
			}
		}(reader)
	}
	finished := make(chan error, 1)
	go func() {
		err := remote.Wait()
		output.Wait()
		finished <- err
	}()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	keepalive := make(chan error, 1)
	var waiting bool
	var keepaliveDeadline <-chan time.Time
	var keepaliveTimer *time.Timer
	defer func() {
		if keepaliveTimer != nil {
			keepaliveTimer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case message := <-incoming:
			if message.input != nil {
				if _, err = remote.Input().Write(message.input); err != nil {
					return
				}
				continue
			}
			switch message.message.Type {
			case "resize":
				if err = remote.Resize(message.message.Cols, message.message.Rows); err != nil {
					return
				}
			case "ack":
				if !writer.ack(message.message.Bytes) {
					return
				}
			case "disconnect":
				return
			default:
				return
			}
		case err = <-finished:
			_ = writer.json(map[string]string{"type": "exit", "message": console.ExitMessage(err)})
			return
		case <-heartbeat.C:
			if connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
				return
			}
			if !waiting {
				waiting = true
				go func() { keepalive <- remote.KeepAlive() }()
				keepaliveTimer = time.NewTimer(10 * time.Second)
				keepaliveDeadline = keepaliveTimer.C
			}
		case err = <-keepalive:
			waiting = false
			keepaliveTimer.Stop()
			keepaliveDeadline = nil
			if err != nil {
				_ = writer.json(map[string]string{"type": "exit", "message": "Соединение с камерой прервано"})
				return
			}
		case <-keepaliveDeadline:
			_ = writer.json(map[string]string{"type": "exit", "message": "Камера перестала отвечать на SSH"})
			return
		}
	}
}

const consoleOutputWindow = 65536

// Output waits for xterm's rendering acknowledgements. Even a continuously
// printing command cannot create an unlimited browser write queue.
type consoleWriter struct {
	ctx      context.Context
	ws       *websocket.Conn
	mu       sync.Mutex
	credit   sync.Mutex
	inflight int
	wake     chan struct{}
}

func newConsoleWriter(ctx context.Context, ws *websocket.Conn) *consoleWriter {
	return &consoleWriter{ctx: ctx, ws: ws, wake: make(chan struct{}, 1)}
}

func (w *consoleWriter) json(message any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return w.ws.WriteJSON(message)
}

func (w *consoleWriter) ack(n int) bool {
	w.credit.Lock()
	defer w.credit.Unlock()
	if n <= 0 || n > w.inflight {
		return false
	}
	w.inflight -= n
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return true
}

func (w *consoleWriter) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		n := min(len(data), 8192)
		for {
			w.credit.Lock()
			if w.inflight+n <= consoleOutputWindow {
				w.inflight += n
				w.credit.Unlock()
				break
			}
			w.credit.Unlock()
			select {
			case <-w.ctx.Done():
				return written, w.ctx.Err()
			case <-w.wake:
			}
		}
		w.mu.Lock()
		_ = w.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		err := w.ws.WriteMessage(websocket.BinaryMessage, data[:n])
		w.mu.Unlock()
		if err != nil {
			return written, err
		}
		written += n
		data = data[n:]
	}
	return written, nil
}
