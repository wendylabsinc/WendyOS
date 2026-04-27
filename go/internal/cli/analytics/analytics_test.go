package analytics

import (
	"testing"

	"github.com/wendylabsinc/wendy/internal/shared/config"
)

// ciEnvVarKeys mirrors env.ciEnvVars; duplicated here to avoid coupling tests
// across packages and to keep this test self-contained when the suite runs in
// a real CI environment.
var ciEnvVarKeys = []string{
	"CI",
	"GITHUB_ACTIONS",
	"GITLAB_CI",
	"BUILDKITE",
	"CIRCLECI",
	"JENKINS_HOME",
	"TF_BUILD",
	"TEAMCITY_VERSION",
}

func clearCIEnv(t *testing.T) {
	t.Helper()
	for _, key := range ciEnvVarKeys {
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
	for _, ciKey := range ciEnvVarKeys {
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
		})
	}
}
