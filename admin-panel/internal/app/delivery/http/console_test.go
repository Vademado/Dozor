package http

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"guard.local/admin/internal/app/console"
	"guard.local/admin/internal/app/repository"
	"guard.local/admin/internal/app/usecase"
	"guard.local/admin/internal/testssh"
)

func consoleHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	journal, err := repository.NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := usecase.New(repository.NewSettingsFile(dir), journal, offlineBridge{}, false, log)
	if err != nil {
		t.Fatal(err)
	}
	return New(service, log).WithConsole(&console.Connector{Keys: console.NewHostKeys(filepath.Join(dir, "ssh_known_hosts"))}), dir
}

func TestConsoleRejectsUnauthenticatedCrossOriginAndInsecureRemoteRequests(t *testing.T) {
	handler, _ := consoleHandler(t)
	routes := handler.Routes(nil, "operator-password", "")
	cases := []struct {
		name, target, origin, peer string
		auth                       bool
		want                       int
	}{
		{"operator auth required", "/api/console/ws", "http://localhost", "127.0.0.1:123", false, 401},
		{"cross-origin rejected", "/api/console/ws", "http://evil.invalid", "127.0.0.1:123", true, 403},
		{"origin required", "/api/console/ws", "", "127.0.0.1:123", true, 403},
		{"credentials in URL forbidden", "/api/console/ws?password=forbidden", "http://localhost", "127.0.0.1:123", true, 403},
		{"plaintext remote terminal forbidden", "/api/console/ws", "http://localhost", "192.168.1.8:123", true, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", "http://localhost"+tc.target, nil)
			request.RemoteAddr = tc.peer
			request.Header.Set("Origin", tc.origin)
			if tc.auth {
				request.SetBasicAuth("admin", "operator-password")
			}
			response := httptest.NewRecorder()
			routes.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("got %d want %d", response.Code, tc.want)
			}
		})
	}
}

func dialConsole(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/console/ws", http.Header{"Origin": []string{server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func nextConsoleJSON(t *testing.T, connection *websocket.Conn) map[string]any {
	t.Helper()
	for {
		kind, payload, err := connection.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind == websocket.BinaryMessage {
			if err = connection.WriteJSON(map[string]any{"type": "ack", "bytes": len(payload)}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		var message map[string]any
		if err = json.Unmarshal(payload, &message); err != nil {
			t.Fatal(err)
		}
		return message
	}
}

func TestConsoleWebSocketSSHTrustInputResizeExitAndNoCredentialLogging(t *testing.T) {
	sshServer, err := testssh.Start("127.0.0.1:0", "test-console-password")
	if err != nil {
		t.Fatal(err)
	}
	defer sshServer.Close()
	handler, dataDir := consoleHandler(t)
	server := httptest.NewServer(handler.Routes(nil, "", ""))
	defer server.Close()
	host, portString, _ := net.SplitHostPort(sshServer.Listener.Addr().String())
	port, _ := strconv.Atoi(portString)
	hello := map[string]any{"type": "connect", "host": host, "port": port, "user": "root", "password": "test-console-password", "cols": 100, "rows": 30}
	first := dialConsole(t, server)
	if err = first.WriteJSON(hello); err != nil {
		t.Fatal(err)
	}
	challenge := nextConsoleJSON(t, first)
	if challenge["type"] != "host_key" || challenge["changed"] != false || sshServer.Attempts.Load() != 0 {
		t.Fatal("credentials sent before host trust", challenge)
	}
	_ = first.Close()
	hello["trust_fingerprint"] = challenge["fingerprint"]
	connection := dialConsole(t, server)
	if err = connection.WriteJSON(hello); err != nil {
		t.Fatal(err)
	}
	if message := nextConsoleJSON(t, connection); message["type"] != "connected" {
		t.Fatal(message)
	}
	if err = connection.WriteMessage(websocket.BinaryMessage, []byte("echo console-test\r\x03")); err != nil {
		t.Fatal(err)
	}
	select {
	case input := <-sshServer.Inputs:
		if string(input) != "echo console-test\r\x03" {
			t.Fatalf("stdin altered: %q", input)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("input not forwarded")
	}
	<-sshServer.Sizes // initial PTY request
	if err = connection.WriteJSON(map[string]any{"type": "resize", "cols": 120, "rows": 40}); err != nil {
		t.Fatal(err)
	}
	select {
	case size := <-sshServer.Sizes:
		if size.Cols != 120 || size.Rows != 40 {
			t.Fatal(size)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resize not forwarded")
	}
	if err = connection.WriteMessage(websocket.BinaryMessage, []byte("exit\r")); err != nil {
		t.Fatal(err)
	}
	if message := nextConsoleJSON(t, connection); message["type"] != "exit" {
		t.Fatal(message)
	}
	for _, file := range []string{"events.jsonl", "settings.json", "ssh_known_hosts"} {
		data, err := os.ReadFile(filepath.Join(dataDir, file))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "test-console-password") || strings.Contains(string(data), "echo console-test") {
			t.Fatal("credentials or commands persisted in", file)
		}
	}
}

func TestConsoleOutputBackpressureAndCancellation(t *testing.T) {
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer ws.Close()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		writer := newConsoleWriter(ctx, ws)
		go func() { _, err := writer.Write(make([]byte, consoleOutputWindow+8192)); done <- err }()
		for {
			var ack consoleMessage
			if ws.ReadJSON(&ack) != nil {
				return
			}
			if !writer.ack(ack.Bytes) {
				return
			}
		}
	}))
	defer server.Close()
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	for bytes := 0; bytes < consoleOutputWindow; {
		_, data, err := connection.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		bytes += len(data)
	}
	select {
	case err := <-done:
		t.Fatal("output window was not bounded", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err = connection.WriteJSON(consoleMessage{Type: "ack", Bytes: 8192}); err != nil {
		t.Fatal(err)
	}
	if _, data, err := connection.ReadMessage(); err != nil || len(data) != 8192 {
		t.Fatal("output did not resume after rendering ACK", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("output remained blocked")
	}
}
