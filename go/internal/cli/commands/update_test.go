package commands

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

type cliUpdateTransport func(*http.Request) (*http.Response, error)

func (f cliUpdateTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCLIUpdateCheckUsesFreshCacheAndThrottles(t *testing.T) {
	oldVersion := version.Version
	version.Version = "2026.09.29-120000"
	t.Cleanup(func() { version.Version = oldVersion })
	t.Setenv("SUDO_UID", "")
	setTempConfig(t, &config.Config{})
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	var calls atomic.Int32
	http.DefaultTransport = cliUpdateTransport(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.String() != githubReleasesURL {
			t.Error("unexpected release URL")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"tag_name":"2026.09.30-120000"}`)), Header: make(http.Header)}, nil
	})
	checkCLIUpdateIfDue(context.Background())
	checkCLIUpdateIfDue(context.Background())
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || cfg.AvailableCLIUpdate != "2026.09.30-120000" || cfg.LastCLIUpdateCheck == "" {
		t.Fatalf("calls=%d update=%q checked=%q", calls.Load(), cfg.AvailableCLIUpdate, cfg.LastCLIUpdateCheck)
	}
	// Simulate a later MCP tick after the 24-hour cache expires.
	cfg.LastCLIUpdateCheck = time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	checkCLIUpdateIfDue(context.Background())
	if calls.Load() != 2 {
		t.Fatal("expired cache did not trigger another check")
	}
	version.Version = "dev"
	if err := config.Save(&config.Config{}); err != nil {
		t.Fatal(err)
	}
	checkCLIUpdateIfDue(context.Background())
	if calls.Load() != 2 {
		t.Fatal("development server contacted release API")
	}
}

func TestCLIUpdateCheckCancellationLeavesCacheUntouched(t *testing.T) {
	oldVersion := version.Version
	version.Version = "2026.09.29-120000"
	t.Cleanup(func() { version.Version = oldVersion })
	t.Setenv("SUDO_UID", "")
	setTempConfig(t, &config.Config{AvailableCLIUpdate: "2026.09.30-120000"})
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	started := make(chan struct{})
	http.DefaultTransport = cliUpdateTransport(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		checkCLIUpdateIfDue(ctx)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("release request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("release request did not cancel")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AvailableCLIUpdate != "2026.09.30-120000" || cfg.LastCLIUpdateCheck != "" {
		t.Fatal("canceled release check changed cache")
	}
}

// TestDueCLIUpdateCheckSkipsDevBuilds asserts that development builds — both the
// literal "dev" default and CI branch builds carrying a "-dev" suffix — never
// trigger the periodic CLI update check (WDY-1770).
func TestDueCLIUpdateCheckSkipsDevBuilds(t *testing.T) {
	original := version.Version
	t.Cleanup(func() { version.Version = original })

	for _, ver := range []string{"dev", "2026.06.30-133859-dev"} {
		t.Run(ver, func(t *testing.T) {
			version.Version = ver
			// Empty LastCLIUpdateCheck would otherwise mark the check as due.
			if dueCLIUpdateCheck(&config.Config{}) {
				t.Errorf("dueCLIUpdateCheck for dev build %q = true, want false", ver)
			}
		})
	}
}

// The background update check used to save the config root loaded at startup,
// seconds later — reverting a default device, pin or login the command itself
// had just saved. It must change only its own two fields on the current config.
func TestPersistCLIUpdateCheckResultKeepsNewerConfig(t *testing.T) {
	setTempConfig(t, &config.Config{})
	if err := config.Save(&config.Config{DefaultDevice: "saved-by-the-command.local"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	if err := persistCLIUpdateCheckResult(now, "", errors.New("offline")); err != nil {
		t.Fatalf("persistCLIUpdateCheckResult: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultDevice != "saved-by-the-command.local" {
		t.Fatalf("the update check reverted DefaultDevice to %q", cfg.DefaultDevice)
	}
	if cfg.LastCLIUpdateCheck != "2026-09-28T12:00:00Z" {
		t.Fatalf("LastCLIUpdateCheck = %q, want 2026-09-28T12:00:00Z", cfg.LastCLIUpdateCheck)
	}
}

func TestCLIUpdateCheckPreservesRotatedOAuthSession(t *testing.T) {
	originalVersion := version.Version
	version.Version = "2026.09.26-062348"
	t.Cleanup(func() { version.Version = originalVersion })

	auth, calls := rotatingOAuthSession(t)
	if err := ensureOAuthAccessToken(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Date(2026, time.September, 27, 12, 0, 0, 0, time.FixedZone("test", 2*60*60))
	if err := persistCLIUpdateCheckResult(checkedAt, "2026.09.27-215032", nil); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || cfg.Auth[0].RefreshToken != "refresh-2" {
		t.Fatal("update check restored the consumed refresh token")
	}
	if cfg.LastCLIUpdateCheck != "2026-09-27T10:00:00Z" || cfg.AvailableCLIUpdate != "2026.09.27-215032" {
		t.Fatalf("update metadata = %q, %q", cfg.LastCLIUpdateCheck, cfg.AvailableCLIUpdate)
	}
}
