package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The same subprocess workflows qualify either standalone World or the
// registered Wendy command. No shell or forwarding script sits between them.
func worldCLIArgs(args []string) []string {
	if os.Getenv("WORLD_CLI_WENDY") == "1" {
		return append([]string{"world"}, args...)
	}
	return args
}

func TestRunHelpUsesCallerStreams(t *testing.T) {
	for _, args := range [][]string{
		nil, {"--help"}, {"query", "--help"}, {"watch", "--help"},
		{"area", "fetch", "--help"}, {"replica", "status", "--help"},
		{"frame", "convert", "--help"}, {"site", "list", "--help"},
		{"package", "fetch", "--help"}, {"evidence", "usage", "--help"},
		{"annotation", "amend", "--help"}, {"zone", "list", "--help"},
		{"task", "send", "--help"}, {"task", "supervise", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			if err := Run(context.Background(), args, Streams{Out: &out, Err: &diagnostic}); err != nil {
				t.Fatal(err)
			}
			if out.Len()+diagnostic.Len() == 0 {
				t.Fatal("help produced no output")
			}
		})
	}
}

func TestRunCancelsActiveGatewayRequest(t *testing.T) {
	cfg, _, _ := taskRelayCredentials(t)
	started := make(chan struct{})
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(requestCanceled)
	}))
	defer server.Close()
	cfg.URL = server.URL
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "reader.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- Run(ctx, []string{"task", "catalog", "--credentials", path}, Streams{}) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not stop request")
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP request outlived invocation")
	}
}

func TestRunCanceledInvocationDoesNotReadCredentials(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, []string{"task", "send", "--credentials", "/missing"}, Streams{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation before file or authority access", err)
	}
}
