package http

import (
	"guard.local/admin/internal/app/relay"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// RelayRoutes exposes only streaming WebSocket, not go2rtc's configuration or
// arbitrary source/FFmpeg API. The source name is fixed by the server.
func RelayRoutes(base string) http.Handler {
	target, _ := url.Parse(base)
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(request *http.Request) {
		director(request)
		request.URL.Path = "/api/ws"
		request.URL.RawQuery = url.Values{"src": {relay.StreamName}}.Encode()
		request.Header.Del("Authorization")
		request.Header.Set("Origin", target.Scheme+"://"+target.Host)
		request.Host = target.Host
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		writeJSON(w, 503, map[string]string{"error": "go2rtc недоступен; перезапустите панель"})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/stream/ws" {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}
