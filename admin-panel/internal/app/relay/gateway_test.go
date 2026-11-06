package relay

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadOnlyObservationAndStatsSurviveProducerReconnect(t *testing.T) {
	registered := true
	requests := 0
	producerID, raw := 1, 100
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" {
			io.WriteString(w, `{"version":"1.9.15"}`)
			return
		}
		if r.URL.Path != "/api/streams" {
			t.Error("unexpected upstream endpoint")
			http.NotFound(w, r)
			return
		}
		requests++
		if r.Method != http.MethodGet || r.URL.Query().Get("src") != StreamName {
			t.Error("observer must only read the existing luckfox stream")
			http.Error(w, "mutation forbidden", 405)
			return
		}
		if !registered {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"producers": []any{map[string]any{"id": producerID, "remote_addr": "camera:554", "receivers": []any{map[string]any{"codec": map[string]string{"codec_name": "h264", "codec_type": "video"}, "bytes": raw, "packets": 10}}}}, "consumers": []any{map[string]int{"id": 1}}})
	}))
	defer server.Close()
	ctx := context.Background()
	gateway := New(server.URL)
	status, err := gateway.Status(ctx)
	if err != nil || !status.Available || status.Registered {
		t.Fatalf("lazy source unavailable: %+v %v", status, err)
	}
	if err = gateway.Start(ctx, "rtsp://camera/live/0"); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatal("Start changed the upstream stream")
	}
	status, err = gateway.Status(ctx)
	if err != nil || !status.Online || status.Bytes != 100 || status.Viewers != 1 || status.Codecs[0] != "H.264" || status.InputFPS != nil {
		t.Fatalf("incorrect stats: %+v %v", status, err)
	}
	raw = 150
	status, _ = gateway.Status(ctx)
	if status.Bytes != 150 {
		t.Fatal("bytes not cumulative")
	}
	producerID = 2
	raw = 20
	status, _ = gateway.Status(ctx)
	if status.Bytes != 170 {
		t.Fatal("reconnect reset bitrate counter")
	}
	registered = false
	status, _ = gateway.Status(ctx)
	if status.Registered {
		t.Fatal("lost registration not detected")
	}
	registered = true
	before := requests
	if err = gateway.Stop(ctx); err != nil || !registered || requests != before {
		t.Fatal("Stop changed the upstream stream")
	}
	gateway.Close()
}

func TestExistingGo2RTCIsReusedAndNotStopped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{"version":"1.9.15"}`) }))
	defer server.Close()
	stop, err := EnsureProcess(context.Background(), server.URL, "missing.exe", "missing.yaml", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if !healthy(context.Background(), server.URL) {
		t.Fatal("user-started gateway was stopped")
	}
}
