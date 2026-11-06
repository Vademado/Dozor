package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestRelayWebSocketPreservesMediaAndRestrictsSource(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ws" || r.URL.Query().Get("src") != "luckfox" || len(r.URL.Query()) != 1 {
			t.Error("unsafe source forwarding")
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("operator credential forwarded upstream")
		}
		connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		kind, data, err := connection.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		if err = connection.WriteMessage(kind, data); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(RelayRoutes(upstream.URL))
	defer proxy.Close()
	address := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/api/stream/ws?src=exec:forbidden&name=other"
	connection, _, err := websocket.DefaultDialer.Dial(address, http.Header{"Origin": []string{proxy.URL}, "Authorization": []string{"Basic private"}})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	want := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p'}
	if err = connection.WriteMessage(websocket.BinaryMessage, want); err != nil {
		t.Fatal(err)
	}
	kind, got, err := connection.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(got) != string(want) {
		t.Fatal("binary media altered by proxy", err)
	}
	response, err := http.Get(proxy.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("go2rtc management API exposed")
	}
}
