package analytics

import (
	"testing"

	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/env"
)

func clearCIEnv(t *testing.T) {
	t.Helper()
	for _, key := range env.CIEnvVars {
		t.Setenv(key, "")
	}
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
