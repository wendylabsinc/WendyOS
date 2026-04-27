// Package analytics provides anonymous usage tracking via PostHog.
package analytics

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/uuid"
	"github.com/posthog/posthog-go"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/env"
	"github.com/wendylabsinc/wendy/internal/shared/version"
)

const (
	posthogAPIKey = "phc_DCgbsvbGPdGhU6GW3CQnEwGCsNNrAHYwMhj4HkhjU4f"
	posthogHost   = "https://us.i.posthog.com"
)

var (
	client     posthog.Client
	enabled    bool
	distinctID string

	// trackHook is set by tests to intercept events before they would be
	// enqueued to PostHog. It is never set in production code.
	trackHook func(event string, properties map[string]string)
)

// SetTrackHookForTesting installs a hook that receives every Track call.
// Pass nil to clear. Intended for tests only.
func SetTrackHookForTesting(fn func(event string, properties map[string]string)) {
	trackHook = fn
}

// Init initializes analytics. If disabled by env var, config, or missing API key,
// tracking is a no-op. Returns true if this is the first run (config.Analytics
// was nil) AND the env var does not override, so the caller can display a notice.
//
// CI environments are hard-disabled here regardless of WENDY_ANALYTICS or the
// stored config — there is no opt-in escape hatch. Real product signal must
// come from human users, not automated runs.
//
// Each disabled-bailout path goes through Disable() so any state from a
// prior Init call (client, distinctID) is cleared. In production Init runs
// once per process, but tests reuse the package-level state across cases.
func Init(cfg *config.Config) (firstRun bool) {
	// Hard kill switch for CI: don't even consider WENDY_ANALYTICS.
	if env.IsCI() {
		Disable()
		return false
	}

	// Env var overrides everything else.
	if !env.Analytics() {
		Disable()
		return false
	}

	// First run: Analytics is nil
	if cfg.Analytics == nil {
		firstRun = true
		enabled = true
	} else {
		enabled = cfg.Analytics.Enabled
	}

	if !enabled {
		Disable()
		return firstRun
	}

	var err error
	distinctID, err = loadOrCreateID()
	if err != nil {
		Disable()
		return firstRun
	}

	client, err = posthog.NewWithConfig(posthogAPIKey, posthog.Config{
		Endpoint: posthogHost,
		Logger:   posthog.StdLogger(log.New(io.Discard, "", 0), false),
	})
	if err != nil {
		Disable()
		return firstRun
	}

	return firstRun
}

// Track sends an analytics event. The PostHog enqueue is a no-op when
// analytics is disabled or uninitialized; the test hook (if any) always
// fires so test assertions can observe the intended payload regardless of
// initialization state.
//
// Caller contract (not enforced by this function): every value in
// `properties` must be anonymous. Allowed: canonical command paths (e.g.
// "wendy device wifi connect"), the top-level command token, success
// booleans, bounded error-class enums, build flags (cli_version,
// is_dev_build), and platform metadata (os, arch). Forbidden: flag values,
// positional arguments, file paths, hostnames, error message text, or
// anything else derived from user input. The function will happily forward
// whatever it is given — reviewers must enforce this at call sites.
func Track(event string, properties map[string]string) {
	if trackHook != nil {
		trackHook(event, properties)
	}
	if !enabled || client == nil {
		return
	}

	props := posthog.NewProperties()
	props.Set("cli_version", version.Version)
	props.Set("os", runtime.GOOS)
	props.Set("arch", runtime.GOARCH)
	for k, v := range properties {
		props.Set(k, v)
	}

	// Enqueue only errors when the buffer is full or the client is closed —
	// neither is actionable from a CLI fire-and-forget path.
	_ = client.Enqueue(posthog.Capture{
		DistinctId: distinctID,
		Event:      event,
		Properties: props,
	})
}

// Close flushes pending events and shuts down the client.
func Close() {
	if client != nil {
		_ = client.Close()
		client = nil
	}
}

// Disable turns off analytics for the current process and closes the client.
func Disable() {
	enabled = false
	Close()
}

// Enabled reports whether analytics is currently enabled.
func Enabled() bool {
	return enabled
}

// EnvOverride reports whether the WENDY_ANALYTICS env var is set to "false".
func EnvOverride() bool {
	return !env.Analytics()
}

func loadOrCreateID() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}

	idPath := filepath.Join(dir, "analytics_id")
	data, err := os.ReadFile(idPath)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			return id, nil
		}
	}

	id := uuid.NewString()
	if err := os.WriteFile(idPath, []byte(id), 0o600); err != nil {
		return "", fmt.Errorf("writing analytics ID: %w", err)
	}
	return id, nil
}
