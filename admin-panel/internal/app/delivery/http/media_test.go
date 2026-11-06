package http

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"guard.local/admin/internal/app/media"
)

type incompatibleBrowser struct{}

func (incompatibleBrowser) Answer(context.Context, string) (media.Answer, error) {
	return media.Answer{}, fmt.Errorf("%w: H.265", media.ErrBrowserCodec)
}
func (incompatibleBrowser) CloseViewer(string) {}

func TestCodecIncompatibilityHasStableErrorCode(t *testing.T) {
	request := httptest.NewRequest("POST", "/api/stream/offer", strings.NewReader(`{"type":"offer","sdp":"test-offer-data"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	MediaRoutes(incompatibleBrowser{}).ServeHTTP(response, request)
	if response.Code != 422 || !strings.Contains(response.Body.String(), `"code":"codec_unsupported"`) {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
}
