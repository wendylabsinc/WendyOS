package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
)

type receivedAnalytics struct {
	Event       string `json:"event"`
	CommandName string `json:"command_name"`
	Success     bool   `json:"success"`
	Raw         string
}

type analyticsTestTransport func(*http.Request) (*http.Response, error)

func (f analyticsTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise the real HTTP sender, not the unconditional Track testing hook. Only
// the telemetry destination is redirected; all other network traffic is blocked.
func receiveCLIAnalytics(t *testing.T) (chan receivedAnalytics, string) {
	t.Helper()
	analytics.Disable()
	analytics.SetTrackHookForTesting(nil)
	clearCIEnv(t)
	t.Setenv("WENDY_ANALYTICS", "true")
	t.Setenv("WENDY_NO_UPDATE_CHECK", "1")
	t.Setenv("WENDY_SECRET_STORE", "file")
	t.Setenv("WENDY_DEVICE", "")
	t.Setenv("SUDO_UID", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", dir)
	events := make(chan receivedAnalytics, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var event receivedAnalytics
		if err := json.Unmarshal(body, &event); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		event.Raw = string(body)
		events <- event
		w.WriteHeader(http.StatusNoContent)
	}))
	destination, _ := url.Parse(srv.URL)
	transport := &http.Transport{}
	previous := http.DefaultTransport
	http.DefaultTransport = analyticsTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "wendy-cli-telemetry-114319063177.us-central1.run.app" || r.URL.Path != "/v1/telemetry/events" {
			return nil, fmt.Errorf("unexpected HTTP request in analytics test")
		}
		copy := r.Clone(r.Context())
		copy.URL.Scheme, copy.URL.Host = destination.Scheme, destination.Host
		return transport.RoundTrip(copy)
	})
	t.Cleanup(func() {
		analytics.Disable()
		http.DefaultTransport = previous
		transport.CloseIdleConnections()
		srv.Close()
	})
	return events, dir
}

func quietAnalyticsRoot() *cobra.Command {
	root := commands.NewRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root
}

func TestInvocationAnalyticsHTTPDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		args          []string
		failure       bool
	}{
		{"bare invocation", "wendy", nil, false},
		{"help flag", "wendy install", []string{"install", "--help"}, false},
		{"version", "wendy", []string{"--version"}, false},
		{"group", "wendy device", []string{"device"}, false},
		{"shell completion", "wendy __complete", []string{"__complete", "private-prefix"}, false},
		{"shell completion alias", "wendy __complete", []string{"__completeNoDesc", "private-prefix"}, false},
		{"unknown command", "wendy", []string{"private-unknown-command"}, true},
		{"unknown subcommand", "wendy device", []string{"device", "private-unknown-command"}, true},
		{"unknown flag", "wendy install", []string{"install", "--private-unknown-flag"}, true},
		{"argument validation", "wendy install", []string{"install", "private-image-path"}, true},
		{"installer error", "wendy install", []string{"install", "/missing/private-image-path", "private-drive"}, true},
		{"legacy installer error", "wendy os install", []string{"os", "install", "/missing/private-image-path", "private-drive"}, true},
		{"normal command", "wendy analytics status", []string{"analytics", "status"}, false},
		{"update early return", "wendy update", []string{"update", "--yes"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, _ := receiveCLIAnalytics(t)
			executed, err := executeInvocation(context.Background(), quietAnalyticsRoot(), tc.args)
			if (err != nil) != tc.failure {
				t.Fatalf("error = %v, want failure=%v", err, tc.failure)
			}
			trackCommand(executed, err, time.Millisecond)
			analytics.Close()
			seen, finished, installStarted := 0, 0, 0
			for len(events) > 0 {
				event := <-events
				if strings.Contains(event.Raw, "private-") {
					t.Fatalf("event leaked command input: %s", event.Raw)
				}
				switch event.Event {
				case "install_started":
					installStarted++
				case "cli_seen":
					seen++
					if event.CommandName != tc.command {
						t.Errorf("launch command = %q, want %q", event.CommandName, tc.command)
					}
				case "command_executed":
					finished++
					if event.CommandName != tc.command || event.Success == tc.failure {
						t.Errorf("wrong completion: %+v", event)
					}
				}
			}
			if seen != 1 || finished != 1 {
				t.Fatalf("received %d launches, %d completions; want one each", seen, finished)
			}
			if strings.Contains(tc.name, "installer error") && installStarted != 1 {
				t.Fatalf("received %d install starts, want one", installStarted)
			}
		})
	}
}

func TestEnablingAnalyticsReportsTheCurrentInvocation(t *testing.T) {
	events, dir := receiveCLIAnalytics(t)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"analytics":{"enabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	executed, err := executeInvocation(context.Background(), quietAnalyticsRoot(), []string{"analytics", "enable"})
	if err != nil {
		t.Fatal(err)
	}
	trackCommand(executed, err, time.Millisecond)
	analytics.Close()
	var finished bool
	for len(events) > 0 {
		event := <-events
		if event.Event == "command_executed" && event.CommandName == "wendy analytics enable" && event.Success {
			finished = true
		}
	}
	if !finished {
		t.Fatal("enable invocation was lost because its startup preference was disabled")
	}
}

func TestHelpDoesNotCountAsProductUse(t *testing.T) {
	for _, args := range [][]string{
		nil, {"--version"}, {"device"}, {"install", "--help"},
		{"discover", "--help"}, {"init", "--help"}, {"auth", "login", "--help"},
		{"__complete", ""},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			events, _ := receiveCLIAnalytics(t)
			executed, err := executeInvocation(context.Background(), quietAnalyticsRoot(), args)
			if err != nil {
				t.Fatal(err)
			}
			trackCommand(executed, nil, time.Millisecond)
			analytics.Close()
			for len(events) > 0 {
				switch event := <-events; event.Event {
				case "first_real_command", "discover_success", "init_success", "auth_success":
					t.Fatalf("help invocation counted as product use: %s", event.Event)
				}
			}
		})
	}
}

func TestInvocationAnalyticsArrivesBeforeCommandReturns(t *testing.T) {
	events, _ := receiveCLIAnalytics(t)
	root := quietAnalyticsRoot()
	root.AddCommand(&cobra.Command{Use: "wait-for-event", GroupID: "settings", RunE: func(*cobra.Command, []string) error {
		select {
		case event := <-events:
			if event.Event != "cli_seen" || event.CommandName != "wendy wait-for-event" {
				t.Fatalf("unexpected event while command is running: %+v", event)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no launch received before command completion")
		}
		return nil
	}})
	if _, err := executeInvocation(context.Background(), root, []string{"wait-for-event"}); err != nil {
		t.Fatal(err)
	}
}

func TestInvocationAnalyticsRespectsOptOutAndConfigFailure(t *testing.T) {
	for _, mode := range []string{"environment", "config", "CI", "malformed config"} {
		t.Run(mode, func(t *testing.T) {
			events, dir := receiveCLIAnalytics(t)
			switch mode {
			case "environment":
				t.Setenv("WENDY_ANALYTICS", "false")
			case "CI":
				t.Setenv("CI", "false") // Presence, not a boolean value, disables analytics.
			case "config", "malformed config":
				body := `{"analytics":{"enabled":false}}`
				if mode == "malformed config" {
					body = `{"analytics":{"enabled":false},`
				}
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			executed, err := executeInvocation(context.Background(), quietAnalyticsRoot(), []string{"install", "--help"})
			trackCommand(executed, err, time.Millisecond)
			analytics.Close()
			if len(events) != 0 {
				t.Fatalf("sent %d events despite %s", len(events), mode)
			}
		})
	}
}

func TestInvocationAnalyticsExcludesLocalDiagnosticsAndHelpers(t *testing.T) {
	for _, args := range [][]string{
		{"doctor", "--help"}, {"update", "--check", "--help"},
		{"update", "--check=invalid", "--help"},
		{"update", "--bogus", "--check"},
		{"update", "--check=invalid", "--check=false"},
		{"install", "plan", "--help"}, {"os", "install", "plan", "--help"},
		{"__session-broker", "--help"}, {"__usb-setup", "--help"},
		{"__t234-write", "--help"}, {"__ble-check", "--help"},
		{"__gpt-relocate", "--help"},
		{"analytics", "disable", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			events, dir := receiveCLIAnalytics(t)
			executed, err := executeInvocation(context.Background(), quietAnalyticsRoot(), args)
			if args[0] != "analytics" && !isReadOnlyInvocation(executed) {
				t.Error("excluded invocation would still run sudo state cleanup")
			}
			trackCommand(executed, err, time.Millisecond)
			analytics.Close()
			if len(events) != 0 {
				t.Fatalf("excluded invocation sent %d events", len(events))
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("excluded invocation wrote config: %v, %v", entries, err)
			}
		})
	}
}
