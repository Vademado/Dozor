package http

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"
)

func basicAuth(password string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if password == "" {
			host := r.Host
			if parsed, _, err := net.SplitHostPort(host); err == nil {
				host = parsed
			}
			if host != "localhost" && host != "127.0.0.1" && host != "[::1]" && host != "::1" {
				writeJSON(w, 403, map[string]string{"error": "локальный доступ разрешён только через localhost или loopback IP"})
				return
			}
		}
		if password != "" {
			username, provided, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(username), []byte("admin")) != 1 || subtle.ConstantTimeCompare([]byte(provided), []byte(password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="Guard console", charset="UTF-8"`)
				http.Error(w, "Требуется вход администратора", 401)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func agentAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			writeJSON(w, 503, map[string]string{"error": "API агента отключён: задайте GUARD_AGENT_TOKEN"})
			return
		}
		authorization := r.Header.Get("Authorization")
		provided := strings.TrimPrefix(authorization, "Bearer ")
		if !strings.HasPrefix(authorization, "Bearer ") || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			writeJSON(w, 401, map[string]string{"error": "неверный токен агента"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				h.log.Error("HTTP handler panicked")
				writeJSON(w, 500, map[string]string{"error": "внутренняя ошибка сервера"})
			}
		}()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// The browser reaches go2rtc on the panel host, not its Docker DNS name.
		playerOrigin := url.URL{Scheme: "http", Host: r.Host}
		if r.TLS != nil {
			playerOrigin.Scheme = "https"
		}
		playerOrigin.Host = net.JoinHostPort(playerOrigin.Hostname(), "1984")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' blob: data:; media-src 'self' blob:; connect-src 'self'; frame-src "+playerOrigin.String()+"; frame-ancestors 'none'; object-src 'none'; base-uri 'none'; form-action 'self'")
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
				writeJSON(w, 403, map[string]string{"error": "запрос с другого сайта запрещён"})
				return
			}
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeJSON(w, 403, map[string]string{"error": "запрос с другого сайта запрещён"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
