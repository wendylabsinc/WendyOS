package livecapture

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type rejectingTransport struct{ calls atomic.Int32 }

func (r *rejectingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return nil, errors.New("ambient transport intercepted scene credential")
}
func TestCaptureCredentialIgnoresAmbientTransport(t *testing.T) {
	var endpointCalls atomic.Int32
	scene := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/samples" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 32) {
			t.Error("wrong explicit endpoint credential")
		}
		endpointCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer scene.Close()
	ambient := &rejectingTransport{}
	previous := http.DefaultTransport
	http.DefaultTransport = ambient
	defer func() { http.DefaultTransport = previous }()
	client, err := New(Config{URL: scene.URL, Token: strings.Repeat("a", 32), RunID: "run-a", Site: "office", Frame: world.FrameRef{ID: "floor", Revision: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Capture(context.Background())
	var response HTTPError
	if !errors.As(err, &response) || response.Status != 503 || endpointCalls.Load() != 1 || ambient.calls.Load() != 0 {
		t.Fatal("capture did not stay on its explicit transport", err)
	}
	client.http.CloseIdleConnections()
}
