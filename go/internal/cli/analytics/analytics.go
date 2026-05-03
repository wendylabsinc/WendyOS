// Package analytics provides anonymous usage tracking via cloud.wendy.sh.
package analytics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/env"
	"github.com/wendylabsinc/wendy/internal/shared/version"
)

const telemetryEndpoint = "https://wendy-cloud-services-nkohwk7hda-uc.a.run.app/v1/telemetry/events"

type eventPayload struct {
	AnonymousID string `json:"anonymous_id"`
	Event       string `json:"event"`
	CommandName string `json:"command_name"`
	CommandRoot string `json:"command_root,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
	Success     bool   `json:"success"`
	ErrorClass  string `json:"error_class,omitempty"`
	CLIVersion  string `json:"cli_version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	IsDevBuild  bool   `json:"is_dev_build"`
}

var (
	client     *http.Client // nil when disabled
	wg         sync.WaitGroup
	enabled    bool
	distinctID string

	// trackHook is set by tests to intercept events before they would be
	// sent to the telemetry endpoint. It is never set in production code.
	trackHook func(event string, properties map[string]string)
)

// SetTrackHookForTesting installs a hook that receives every Track call.
// Pass nil to clear. Intended for tests only.
func SetTrackHookForTesting(fn func(event string, properties map[string]string)) {
	trackHook = fn
}

// Init initializes analytics. If disabled by env var, config, or CI environment,
// tracking is a no-op. Returns true if this is the first run (config.Analytics
// was nil) AND the env var does not override, so the caller can display a notice.
//
// CI environments are hard-disabled here regardless of WENDY_ANALYTICS or the
// stored config — there is no opt-in escape hatch. Real product signal must
// come from human users, not automated runs.
func Init(cfg *config.Config) (firstRun bool) {
	if env.IsCI() {
		Disable()
		return false
	}
	if !env.Analytics() {
		Disable()
		return false
	}

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

	client = &http.Client{Timeout: 5 * time.Second}
	return firstRun
}

// Track sends an analytics event. The HTTP send is a no-op when analytics is
// disabled or uninitialized; the test hook (if any) always fires so test
// assertions can observe the intended payload regardless of initialization state.
func Track(event string, properties map[string]string) {
	if trackHook != nil {
		trackHook(event, properties)
	}
	if !enabled || client == nil {
		return
	}

	payload := eventPayload{
		AnonymousID: distinctID,
		Event:       event,
		CommandName: properties["command_name"],
		CommandRoot: properties["command_root"],
		DurationMS:  parseInt64(properties["duration_ms"]),
		Success:     parseBool(properties["success"]),
		ErrorClass:  properties["error_class"],
		CLIVersion:  version.Version,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		IsDevBuild:  version.Version == "dev",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	c := client
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := c.Post(telemetryEndpoint, "application/json", bytes.NewReader(body))
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
}

// Close waits for any in-flight events to finish sending and resets the client.
func Close() {
	wg.Wait()
	client = nil
}

// Disable turns off analytics for the current process and flushes any pending events.
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

func parseInt64(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

func parseBool(s string) bool {
	return strings.EqualFold(s, "true")
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
