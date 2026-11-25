package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSelectedClientSignsReadBodyAndValidatesReplies(t *testing.T) {
	cfg, v := credentials(t)
	mode := "ok"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var signed authority.SignedRequest
		header, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		if err = json.Unmarshal(header, &signed); err != nil {
			t.Error(err)
			return
		}
		if _, err = v.Verify(signed, "read", body); err != nil {
			t.Error(err)
			http.Error(w, "denied", 401)
			return
		}
		if r.Method != "POST" || (r.URL.Path != "/v1/query" && r.URL.Path != "/v1/watch/selected") {
			t.Error("wrong selected endpoint")
		}
		if mode == "conflict" {
			http.Error(w, "changed", 409)
			return
		}
		if mode == "expired" {
			http.Error(w, "expired", 410)
			return
		}
		out := selection.Response{Version: 1, Components: []world.Component{}, Revision: "revision", Complete: true, ObservedAt: time.Now()}
		switch mode {
		case "unselected":
			out.Components = []world.Component{{Key: world.Key{Entity: "outside"}}}
		case "duplicate":
			c := world.Component{Key: world.Key{Entity: "inside"}}
			out.Components = []world.Component{c, c}
		case "incomplete":
			out.Complete = false
			out.NextCursor = "next"
		case "null":
			out.Components = nil
		case "query-timeout":
			out.TimedOut = true
		case "watch-changed-timeout":
			out.TimedOut = true
			out.Revision = "changed"
		case "unknown":
			w.Write([]byte(`{"unknown":true}`))
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer server.Close()
	cfg.URL = server.URL
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := selection.Request{Selection: selection.Selection{Entities: []string{"inside"}}}
	if _, err = c.QuerySelected(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"unselected", "duplicate", "incomplete", "null", "query-timeout", "unknown"} {
		mode = value
		if _, err = c.QuerySelected(context.Background(), request); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
	mode = "watch-changed-timeout"
	if _, err = c.WatchSelected(context.Background(), selection.WatchRequest{Revision: "revision"}); err == nil {
		t.Fatal("false timeout")
	}
	mode = "conflict"
	if _, err = c.QuerySelected(context.Background(), request); !errors.Is(err, ErrSelectedViewChanged) {
		t.Fatal(err)
	}
	mode = "expired"
	if _, err = c.QuerySelected(context.Background(), request); !errors.Is(err, ErrSelectedCursorExpired) {
		t.Fatal(err)
	}
	before := calls
	request.PageSize = 129
	if _, err = c.QuerySelected(context.Background(), request); err == nil || calls != before {
		t.Fatal("invalid query sent")
	}
}
