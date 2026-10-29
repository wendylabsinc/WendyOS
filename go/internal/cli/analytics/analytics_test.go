package analytics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

func clearCIEnv(t *testing.T) {
	t.Helper()
	for _, key := range env.CIEnvVars {
		t.Setenv(key, "")
	}
}

// Isolate the shared user identity independently of device configuration.
func isolateIdentityHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".wendy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func enableTestDelivery(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	Close()
	server := httptest.NewServer(handler)
	stateMu.Lock()
	previousEndpoint := telemetryEndpoint
	telemetryEndpoint = server.URL
	enabled = true
	distinctID = "analytics-test-id"
	client = server.Client()
	stateMu.Unlock()
	t.Cleanup(func() {
		Close()
		stateMu.Lock()
		enabled = false
		distinctID = ""
		telemetryEndpoint = previousEndpoint
		stateMu.Unlock()
		server.Close()
	})
	return server
}

func TestDisabledViaEnvVar(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("WENDY_ANALYTICS", "false")
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())

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
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())

	cfg := &config.Config{
		Analytics: &config.AnalyticsConfig{Enabled: false},
	}
	Init(cfg)

	if Enabled() {
		t.Error("expected analytics to be disabled via config")
	}
}

func TestEnabledByDefaultWhenNil(t *testing.T) {
	isolateIdentityHome(t)
	clearCIEnv(t)
	t.Setenv("WENDY_ANALYTICS", "")
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Cleanup(Disable)

	cfg := &config.Config{
		Analytics: nil,
	}
	firstRun := Init(cfg)

	if !firstRun {
		t.Error("expected firstRun to be true when Analytics is nil")
	}
	if !Enabled() {
		t.Error("expected analytics to be enabled when Analytics is nil")
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
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())

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
			t.Setenv("WENDY_CONFIG_DIR", t.TempDir())

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
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())

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

func TestFlushWaitsForDeliveryAndKeepsTrackingEnabled(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseResponse) })
	bodies := make(chan eventPayload, 2)
	enableTestDelivery(t, func(w http.ResponseWriter, r *http.Request) {
		var body eventPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode event payload: %v", err)
		}
		bodies <- body
		if body.Event == "command_started" {
			close(requestStarted)
			<-releaseResponse
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Unblock the handler before the delivery fixture cleans up if an assertion
	// fails while the first request is still in flight.
	t.Cleanup(release)

	Track("command_started", map[string]string{"command_name": "wendy install"})
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("start event never reached the receiver")
	}

	flushed := make(chan struct{})
	go func() {
		Flush()
		close(flushed)
	}()
	select {
	case <-flushed:
		t.Fatal("Flush returned before the telemetry request completed")
	case <-time.After(20 * time.Millisecond):
	}

	release()
	select {
	case <-flushed:
	case <-time.After(time.Second):
		t.Fatal("Flush did not return after the telemetry request completed")
	}

	// Failed elevation returns to the same process. Its final event must still
	// use the initialized client after the pre-elevation flush.
	Track("command_executed", map[string]string{
		"command_name": "wendy install",
		"success":      "false",
		"error_class":  "permission_denied",
	})
	Flush()
	if len(bodies) != 2 {
		t.Fatalf("expected start and final events across Flush, got %d", len(bodies))
	}
	if got := <-bodies; got.Event != "command_started" {
		t.Fatalf("first event = %q, want command_started", got.Event)
	}
	if got := <-bodies; got.Event != "command_executed" || got.ErrorClass != "permission_denied" {
		t.Fatalf("final event = %+v, want failed command_executed", got)
	}
}

func TestCloseStopsAdmissionBeforeWaitingForDelivery(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseResponse) })
	var requests atomic.Int64
	enableTestDelivery(t, func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(requestStarted)
		}
		<-releaseResponse
		w.WriteHeader(http.StatusNoContent)
	})
	t.Cleanup(release)

	Track("command_started", map[string]string{"command_name": "wendy install"})
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("start event never reached the receiver")
	}
	closed := make(chan struct{})
	go func() {
		Close()
		close(closed)
	}()
	deadline := time.Now().Add(time.Second)
	for trackingEnabled() {
		if time.Now().After(deadline) {
			t.Fatal("Close kept accepting events while waiting for delivery")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-closed:
		t.Fatal("Close returned while a delivery was pending")
	default:
	}

	// Background producers can still call Track while shutdown drains existing
	// requests. None may start a request after Close stopped admission.
	var producers sync.WaitGroup
	for range 32 {
		producers.Go(func() {
			Track("late_event", map[string]string{"command_name": "wendy install"})
		})
	}
	producers.Wait()
	release()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after delivery completed")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected only the admitted request, got %d", got)
	}
}

func TestConcurrentTrackFlushAndClose(t *testing.T) {
	var requests atomic.Int64
	enableTestDelivery(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	Track("command_started", map[string]string{"command_name": "wendy install"})

	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			<-start
			for range 4 {
				Track("progress", map[string]string{"command_name": "wendy install"})
				Flush()
				_ = Enabled()
			}
		})
	}
	workers.Go(func() {
		<-start
		Close()
	})
	close(start)
	workers.Wait()
	Close()
	if got := requests.Load(); got < 1 {
		t.Fatal("the event admitted before Close was lost")
	}
	if pending := pendingDeliveries(false, false); len(pending) != 0 {
		t.Fatalf("%d deliveries still pending after shutdown", len(pending))
	}
	if trackingEnabled() {
		t.Fatal("tracking remained active after Close")
	}
}

func TestDeliveryCallbackCanTrackAfterFlush(t *testing.T) {
	var requests atomic.Int64
	enableTestDelivery(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	track("first", nil, func() { Track("followup", nil) })
	Flush()
	// The callback's new event may have been admitted after Flush's snapshot.
	Flush()
	if got := requests.Load(); got != 2 {
		t.Fatalf("expected initial and callback events, got %d", got)
	}
}

func TestLoadOrCreateIDReadsExistingIdentityWithoutLock(t *testing.T) {
	dir := isolateIdentityHome(t)
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	const want = "69eedfb8-a7d5-43c5-b61d-3083caa38f63"
	if err := os.WriteFile(filepath.Join(dir, "analytics_id"), []byte(want), 0o400); err != nil {
		t.Fatal(err)
	}
	// A directory at the lock path prevents lock acquisition on every platform.
	if err := os.Mkdir(filepath.Join(dir, "analytics_id.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	if id, err := loadOrCreateID(); err != nil || id != want {
		t.Fatalf("existing identity = %q, %v; want %q without a writable lock", id, err, want)
	}
}

func TestLoadOrCreateIDSharedAcrossConfigDirectories(t *testing.T) {
	for _, tc := range []struct {
		name, shared, legacy string
	}{
		{name: "new identity"},
		{name: "preserve home identity", shared: "69eedfb8-a7d5-43c5-b61d-3083caa38f63"},
		{name: "migrate config identity", legacy: "805840d4-130b-4317-9c43-a2c28ec21246"},
		{name: "home wins over config", shared: "69eedfb8-a7d5-43c5-b61d-3083caa38f63", legacy: "805840d4-130b-4317-9c43-a2c28ec21246"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolateIdentityHome(t)
			legacyDir := t.TempDir()
			t.Setenv("WENDY_CONFIG_DIR", legacyDir)
			for path, id := range map[string]string{
				filepath.Join(dir, "analytics_id"):       tc.shared,
				filepath.Join(legacyDir, "analytics_id"): tc.legacy,
			} {
				if id != "" {
					if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			id, err := loadOrCreateID()
			if err != nil || id == "" {
				t.Fatalf("first identity = %q, %v", id, err)
			}
			want := tc.shared
			if want == "" {
				want = tc.legacy
			}
			if want != "" && id != want {
				t.Fatalf("identity = %q, want preserved %q", id, want)
			}
			stored, err := os.ReadFile(filepath.Join(dir, "analytics_id"))
			if err != nil || string(stored) != id {
				t.Fatalf("shared identity was not persisted: %q, %v", stored, err)
			}
			if tc.legacy != "" {
				stored, err := os.ReadFile(filepath.Join(legacyDir, "analytics_id"))
				if err != nil || string(stored) != tc.legacy {
					t.Fatalf("legacy identity changed: %q, %v", stored, err)
				}
			}
			otherDir := t.TempDir()
			for _, configDir := range []string{otherDir, "", legacyDir} {
				t.Setenv("WENDY_CONFIG_DIR", configDir)
				if got, err := loadOrCreateID(); err != nil || got != id {
					t.Fatalf("config %q identity = %q, %v; want %q", configDir, got, err, id)
				}
			}
			if _, err := os.Stat(filepath.Join(otherDir, "analytics_id")); !os.IsNotExist(err) {
				t.Fatalf("created config-specific identity: %v", err)
			}
		})
	}
}

func TestLoadOrCreateIDDoesNotReplaceUnreadableIdentity(t *testing.T) {
	for _, location := range []string{"shared", "legacy"} {
		t.Run(location, func(t *testing.T) {
			dir := isolateIdentityHome(t)
			legacyDir := t.TempDir()
			t.Setenv("WENDY_CONFIG_DIR", legacyDir)
			brokenDir := dir
			if location == "legacy" {
				brokenDir = legacyDir
			}
			if err := os.Mkdir(filepath.Join(brokenDir, "analytics_id"), 0o700); err != nil {
				t.Fatal(err)
			}
			if id, err := loadOrCreateID(); err == nil || id != "" {
				t.Fatalf("unreadable identity replaced with %q: %v", id, err)
			}
		})
	}
}

func TestLoadOrCreateIDConcurrentFirstLaunchesShareIdentity(t *testing.T) {
	dir := isolateIdentityHome(t)
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	const count = 32
	ids := make(chan string, count)
	errs := make(chan error, count)
	start := make(chan struct{})
	var launches sync.WaitGroup
	for range count {
		launches.Go(func() {
			<-start
			id, err := loadOrCreateID()
			ids <- id
			errs <- err
		})
	}
	close(start)
	launches.Wait()
	stored, err := os.ReadFile(filepath.Join(dir, "analytics_id"))
	if err != nil {
		t.Fatalf("read persisted identity: %v", err)
	}
	if len(stored) == 0 {
		t.Fatal("persisted identity is empty")
	}
	for range count {
		if err := <-errs; err != nil {
			t.Errorf("concurrent first launch failed: %v", err)
		}
		if got := <-ids; got != string(stored) {
			t.Errorf("launch identity = %q, persisted identity = %q", got, stored)
		}
	}
}

func TestLoadOrCreateIDRechecksAfterWaitingForFirstWriter(t *testing.T) {
	dir := isolateIdentityHome(t)
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	release, err := flock.Acquire(filepath.Join(dir, "analytics_id.lock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	type result struct {
		id  string
		err error
	}
	loaded := make(chan result, 1)
	go func() {
		id, err := loadOrCreateID()
		loaded <- result{id, err}
	}()
	select {
	case got := <-loaded:
		t.Fatalf("identity creation did not wait for the first writer: %+v", got)
	case <-time.After(30 * time.Millisecond):
	}
	const want = "69eedfb8-a7d5-43c5-b61d-3083caa38f63"
	if err := os.WriteFile(filepath.Join(dir, "analytics_id"), []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case got := <-loaded:
		if got.err != nil || got.id != want {
			t.Fatalf("identity after first writer = %+v, want %s", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("identity creation did not resume after the first writer released the lock")
	}
}

func TestTrackSendsOnlyAllowedProperties(t *testing.T) {
	bodies := make(chan eventPayload, 1)
	enableTestDelivery(t, func(w http.ResponseWriter, r *http.Request) {
		var body eventPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies <- body
	})
	Track("deploy_completed", map[string]string{"command_name": "wendy run", "duration_ms": "1234", "success": "true", "deploy_transport": "chunk", "deploy_build_ms": "123", "private_path": "/Users/alice/secret", "deploy_device_type": "alice-private-device", "deploy_push_ms": "secret", "deploy_fallback": strings.Repeat("x", 200)})
	Close()
	body := <-bodies
	if len(body.Properties) != 2 || body.Properties["deploy_transport"] != "chunk" || body.Properties["deploy_build_ms"] != "123" {
		t.Fatalf("unsafe or missing properties: %v", body.Properties)
	}
	if body.SchemaVersion != 2 || body.EventID == "" || body.AttemptID == "" || body.OccurredAt.IsZero() {
		t.Fatalf("missing v2 envelope: %+v", body)
	}
	if body.DurationMS != 1234 {
		t.Fatalf("duration = %d", body.DurationMS)
	}
}

func TestTrackOmitsPropertiesForCoreOnlyEvents(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	enableTestDelivery(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies <- body
	})
	Track("command_executed", map[string]string{"command_name": "wendy run", "success": "true"})
	Close()
	if _, ok := (<-bodies)["properties"]; ok {
		t.Fatal("a command event without extra properties must not send a properties object")
	}
}
