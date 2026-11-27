package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/patrolreplica"
)

func TestStatusReaderNeverRedirectsOrCallsControl(t *testing.T) {
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/status" {
			t.Error("unexpected operation")
		}
		http.Redirect(w, r, target.URL+"/status", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	if _, e := status(context.Background(), source.URL+"/status"); e == nil {
		t.Fatal("redirect followed")
	}
	for _, url := range []string{source.URL + "/control", source.URL + "/status?control=stop", "http://example.org/status"} {
		if _, e := status(context.Background(), url); e == nil {
			t.Fatal("nonlocal/nonstatus URL allowed")
		}
	}
	if calls != 0 {
		t.Fatal("redirect target contacted")
	}
}
func TestReadBridgeRejectsMotionCommandWithoutTransport(t *testing.T) {
	out := t.TempDir()
	for _, p := range []string{"commands", "receipts", "media"} {
		os.Mkdir(filepath.Join(out, p), 0700)
	}
	c := patrolreplica.Config{RunID: "run"}
	request := `{"schema":"wendy.nav.patrol.debrief-command.v1","run_id":"run","action":"recall","report_id":"x","digest":"x"}`
	os.WriteFile(filepath.Join(out, "commands/one.json"), []byte(request), 0600)
	if e := commands(context.Background(), out, c, nil, patrolreplica.View{}); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(out, "receipts/one.json"))
	if e != nil || !strings.Contains(string(b), `"accepted":false`) || !strings.Contains(string(b), "unsupported read-only") {
		t.Fatal(string(b), e)
	}
}
