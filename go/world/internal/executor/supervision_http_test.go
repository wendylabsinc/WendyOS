package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPDeadlineAdapterUsesDistinctBoundedProtocol(t *testing.T) {
	var got map[string]json.RawMessage
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/command/bounded" || r.Method != "POST" {
			t.Errorf("wrong protocol %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(sample())
	}))
	defer server.Close()
	adapter, err := NewHTTPAdapter(server.URL, strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(200 * time.Millisecond).UnixMilli()
	if err = adapter.CommandUntil(context.Background(), "session", 7, .2, 100*time.Millisecond, deadline); err != nil {
		t.Fatal(err)
	}
	var actualDeadline int64
	if err = json.Unmarshal(got["not_after_unix_ms"], &actualDeadline); err != nil || actualDeadline != deadline || len(got) != 5 {
		t.Fatal("deadline not bound on wire", got, err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	for _, expired := range []int64{0, time.Now().Add(-time.Millisecond).UnixMilli(), time.Now().Add(time.Second).UnixMilli()} {
		if err = adapter.CommandUntil(context.Background(), "session", 8, .2, 100*time.Millisecond, expired); err == nil {
			t.Fatal("bad deadline sent")
		}
	}
	if calls != 1 {
		t.Fatal("invalid command reached transport", calls)
	}
}

func TestHTTPDeadlineAdapterDoesNotFallbackToLegacyEndpoint(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/command/bounded" {
			t.Errorf("legacy fallback used: %s", r.URL.Path)
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	adapter, _ := NewHTTPAdapter(server.URL, strings.Repeat("a", 32))
	if err := adapter.CommandUntil(context.Background(), "session", 1, .2, 100*time.Millisecond, time.Now().Add(200*time.Millisecond).UnixMilli()); err == nil || calls != 1 {
		t.Fatal("legacy adapter accepted supervised command", err, calls)
	}
}
