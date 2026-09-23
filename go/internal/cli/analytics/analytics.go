// Package analytics provides anonymous usage tracking via cloud.wendy.dev.
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
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

const defaultTelemetryEndpoint = "https://wendy-cli-telemetry-114319063177.us-central1.run.app/v1/telemetry/events"

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
	// telemetryEndpoint is a variable so delivery behavior can be exercised
	// against an httptest server without sending test events to production.
	telemetryEndpoint = defaultTelemetryEndpoint

	// trackHook is set by tests to intercept events before they would be
	// sent to the telemetry endpoint. It is never set in production code.
	trackHook func(event string, properties map[string]string)
)

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
	track(event, properties, nil)
}

// track sends an event and calls onDelivered only after the receiver returns a
// 2xx response. Delivery remains best-effort: failures never affect the command
// result, but callers such as one-time milestones can defer local acknowledgement
// until the receiver has persisted the event.
func track(event string, properties map[string]string, onDelivered func()) {
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
		IsDevBuild:  version.IsDev(version.Version),
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
		_ = resp.Body.Close()
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return
		}
		if onDelivered != nil {
			onDelivered()
		}
	}()
}

const milestonesFileName = "milestones"

// milestoneSent reports whether name was already recorded in dir/milestones.
func milestoneSent(dir, name string) bool {
	data, err := os.ReadFile(filepath.Join(dir, milestonesFileName))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}

// recordMilestone appends name to dir/milestones.
func recordMilestone(dir, name string) error {
	f, err := os.OpenFile(filepath.Join(dir, milestonesFileName),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(name + "\n")
	return err
}

// TrackMilestoneOnceInDir emits the named milestone event exactly once for the
// given state dir. The dir is a parameter so tests can use a temp dir.
func TrackMilestoneOnceInDir(dir, name string) {
	if milestoneSent(dir, name) {
		return
	}
	track(name, map[string]string{"command_name": name}, func() {
		_ = recordMilestone(dir, name)
	})
}

// TrackMilestoneOnce emits the named milestone event exactly once per
// installation. It is a no-op when analytics is disabled. Milestone state lives
// alongside analytics_id in the config dir.
func TrackMilestoneOnce(name string) {
	if !enabled {
		return
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return
	}
	TrackMilestoneOnceInDir(dir, name)
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
