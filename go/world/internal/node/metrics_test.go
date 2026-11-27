package node

import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestMetricsUseOnlyStaticClasses(t *testing.T) {
	m := runtimeobs.New()
	n := &Node{cfg: Config{Metrics: m}}
	h := n.measured(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "same")
		w.WriteHeader(403)
		_, _ = w.Write([]byte("refused"))
	}))
	for _, path := range []string{"/v1/tasks/supervision/challenge", "/v1/tasks/supervision/ack", "/v1/tasks/evaluate", "/v1/tasks/stop", "/secret-principal?key=never-record"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		if w.Code != 403 || w.Body.String() != "refused" || w.Header().Get("X-Test") != "same" {
			t.Fatal("metrics changed response")
		}
	}
	s := m.Snapshot()
	for _, d := range s.Durations {
		if strings.HasPrefix(d.Name, "http.") && d.Count != 1 {
			t.Fatal(d)
		}
	}
	for _, c := range s.Counters {
		if c.Name == "http.failures" && c.Value != 5 {
			t.Fatal(c)
		}
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "never-record") {
		t.Fatal("request data entered metrics")
	}
}
func TestRequestMetricsCountCancellationAndPanic(t *testing.T) {
	m := runtimeobs.New()
	n := &Node{cfg: Config{Metrics: m}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.measured(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil).WithContext(ctx))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic swallowed")
			}
		}()
		n.measured(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("failure") })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	for _, c := range m.Snapshot().Counters {
		if c.Name == "http.failures" && c.Value != 2 {
			t.Fatal(c)
		}
	}
}
