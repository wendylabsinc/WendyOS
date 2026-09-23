package analytics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

func clearCIEnv(t *testing.T) {
	t.Helper()
	for _, key := range env.CIEnvVars {
		t.Setenv(key, "")
	}
}

func enableTestDelivery(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	Close()
	server := httptest.NewServer(handler)
	previousEndpoint := telemetryEndpoint
	telemetryEndpoint = server.URL
	enabled = true
	distinctID = "analytics-test-id"
	client = server.Client()
	t.Cleanup(func() {
		Close()
		enabled = false
		distinctID = ""
		telemetryEndpoint = previousEndpoint
		server.Close()
	})
	return server
}

func TestDisabledViaEnvVar(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("WENDY_ANALYTICS", "false")
	t.Setenv("HOME", t.TempDir())

	cfg := &config.Config{
		Analytics: &config.AnalyticsConfig{Enabled: true},
	}
	Init(cfg)

	if Enabled() {
		t.Error("expected analytics to be disabled via env var")
	}
}

func TestDisabledViaConfig(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("WENDY_ANALYTICS", "")
	t.Setenv("HOME", t.TempDir())

	cfg := &config.Config{
		Analytics: &config.AnalyticsConfig{Enabled: false},
	}
	Init(cfg)

	if Enabled() {
		t.Error("expected analytics to be disabled via config")
	}
}

func TestEnabledByDefaultWhenNil(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("WENDY_ANALYTICS", "")
	t.Setenv("HOME", t.TempDir())

	cfg := &config.Config{
		Analytics: nil,
	}
	firstRun := Init(cfg)

	if !firstRun {
		t.Error("expected firstRun to be true when Analytics is nil")
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("WENDY_ANALYTICS", "false")
	if !EnvOverride() {
		t.Error("expected EnvOverride to return true")
	}

	t.Setenv("WENDY_ANALYTICS", "")
	if EnvOverride() {
		t.Error("expected EnvOverride to return false")
	}
}

func TestTrackNoOpWhenDisabled(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("WENDY_ANALYTICS", "false")
	t.Setenv("HOME", t.TempDir())

	cfg := &config.Config{}
	Init(cfg)

	// Should not panic
	Track("test_event", map[string]string{"key": "value"})
	Close()
}

// TestInitDisabledInCI is the load-bearing test for the "no analytics in CI,
// ever" rule. Even when the user has explicitly opted in via env var AND has
// an enabled stored config, the presence of any CI marker must hard-disable
// the analytics client.
func TestInitDisabledInCI(t *testing.T) {
	for _, ciKey := range env.CIEnvVars {
		t.Run(ciKey, func(t *testing.T) {
			clearCIEnv(t)
			t.Setenv(ciKey, "1")
			t.Setenv("WENDY_ANALYTICS", "true")
			t.Setenv("HOME", t.TempDir())

			cfg := &config.Config{
				Analytics: &config.AnalyticsConfig{Enabled: true},
			}
			firstRun := Init(cfg)

			if firstRun {
				t.Errorf("Init must return firstRun=false in CI (%s set)", ciKey)
			}
			if Enabled() {
				t.Errorf("analytics must not be enabled in CI (%s set), even with WENDY_ANALYTICS=true and config.enabled=true", ciKey)
			}
			// Structural invariant: the HTTP client must not exist when
			// disabled. Without this, a future regression that flips the
			// hook-vs-gate ordering inside Track could re-enable sends
			// silently — `Enabled()` alone wouldn't catch it.
			if client != nil {
				t.Errorf("http client must be nil in CI; got %T", client)
			}
		})
	}
}

// TestTrackHookFiresEvenWhenDisabled documents that the test hook is a
// caller-visible seam: it fires on every Track call regardless of whether
// analytics is enabled. The HTTP send is the gated side effect, not
// the hook. Tests rely on this to inspect intended payloads without
// making real network requests.
func TestTrackHookFiresEvenWhenDisabled(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("WENDY_ANALYTICS", "false")
	t.Setenv("HOME", t.TempDir())

	Init(&config.Config{}) // disabled
	if Enabled() {
		t.Fatal("test setup: Init should have left analytics disabled")
	}

	var got []string
	SetTrackHookForTesting(func(event string, _ map[string]string) {
		got = append(got, event)
	})
	t.Cleanup(func() { SetTrackHookForTesting(nil) })

	Track("synthetic", map[string]string{"k": "v"})
	if len(got) != 1 || got[0] != "synthetic" {
		t.Errorf("hook must fire when disabled; got %v", got)
	}
	if client != nil {
		t.Error("client must remain nil when disabled")
	}
}

func TestTrackMilestoneOnceInDir_RecordsAfter2xx(t *testing.T) {
	dir := t.TempDir()
	requests := 0
	server := enableTestDelivery(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	})

	TrackMilestoneOnceInDir(dir, "first_deploy_success")
	Close()

	// Re-enable the test client after Close. The second call should return from
	// the persisted milestone check without issuing another request.
	client = server.Client()
	TrackMilestoneOnceInDir(dir, "first_deploy_success")
	Close()

	if requests != 1 {
		t.Fatalf("expected milestone to be delivered exactly once, got %d requests", requests)
	}
	data, err := os.ReadFile(filepath.Join(dir, "milestones"))
	if err != nil {
		t.Fatalf("reading milestones file: %v", err)
	}
	if string(data) != "first_deploy_success\n" {
		t.Fatalf("unexpected milestones file contents: %q", string(data))
	}
}

func TestTrackMilestoneOnceInDir_RetriesAfterNon2xx(t *testing.T) {
	dir := t.TempDir()
	statusCode := http.StatusInternalServerError
	requests := 0
	server := enableTestDelivery(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(statusCode)
	})

	TrackMilestoneOnceInDir(dir, "first_deploy_success")
	Close()

	if milestoneSent(dir, "first_deploy_success") {
		t.Fatal("milestone must remain pending after a non-2xx response")
	}
	if _, err := os.Stat(filepath.Join(dir, milestonesFileName)); !os.IsNotExist(err) {
		t.Fatalf("milestone file should not be created after failed delivery; err=%v", err)
	}

	statusCode = http.StatusNoContent
	client = server.Client()
	TrackMilestoneOnceInDir(dir, "first_deploy_success")
	Close()

	if !milestoneSent(dir, "first_deploy_success") {
		t.Fatal("milestone should be recorded after a later successful delivery")
	}
	if requests != 2 {
		t.Fatalf("expected one failed attempt and one retry, got %d requests", requests)
	}
}

func TestTrackClassifiesSuffixedDevBuild(t *testing.T) {
	var got eventPayload
	enableTestDelivery(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode event payload: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	previousVersion := version.Version
	version.Version = "2026.08.24-171500-dev"
	t.Cleanup(func() { version.Version = previousVersion })

	Track("command_executed", map[string]string{
		"command_name": "wendy version",
		"success":      "true",
	})
	Close()

	if !got.IsDevBuild {
		t.Fatalf("is_dev_build = false for suffixed dev version %q", version.Version)
	}
}

func TestCloseWaitsForInFlightDelivery(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	enableTestDelivery(t, func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseResponse
		w.WriteHeader(http.StatusNoContent)
	})

	Track("command_executed", map[string]string{
		"command_name": "wendy version",
		"success":      "true",
	})
	<-requestStarted

	closed := make(chan struct{})
	go func() {
		Close()
		close(closed)
	}()

	select {
	case <-closed:
		t.Fatal("Close returned before the telemetry request completed")
	default:
	}

	close(releaseResponse)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after the telemetry request completed")
	}
}
