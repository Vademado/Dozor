package http

import (
	"context"
	"errors"
	"net/http"

	"guard.local/admin/internal/app/media"
)

type MediaService interface {
	Answer(context.Context, string) (media.Answer, error)
	CloseViewer(string)
}

// MediaRoutes exchanges SDP over same-origin HTTP. Video is sent by Pion as SRTP.
func MediaRoutes(engine MediaService) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/stream/offer", func(w http.ResponseWriter, r *http.Request) {
		var offer struct {
			Type string `json:"type"`
			SDP  string `json:"sdp"`
		}
		if err := readJSON(w, r, &offer); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if offer.Type != "offer" || len(offer.SDP) < 10 {
			writeJSON(w, 400, map[string]string{"error": "ожидается SDP offer"})
			return
		}
		answer, err := engine.Answer(r.Context(), offer.SDP)
		if err != nil {
			if errors.Is(err, media.ErrBrowserCodec) {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "code": "codec_unsupported"})
				return
			}
			writeJSON(w, 503, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, answer)
	})
	mux.HandleFunc("DELETE /api/stream/viewers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if len(id) != 32 {
			writeJSON(w, 400, map[string]string{"error": "неверный ID просмотра"})
			return
		}
		engine.CloseViewer(id)
		w.WriteHeader(204)
	})
	return mux
}
