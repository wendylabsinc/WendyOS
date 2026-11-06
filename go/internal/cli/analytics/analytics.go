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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

const defaultTelemetryEndpoint = "https://wendy-cli-telemetry-114319063177.us-central1.run.app/v1/telemetry/events"

type eventPayload struct {
	SchemaVersion int       `json:"schema_version"`
	EventID       string    `json:"event_id"`
	OccurredAt    time.Time `json:"occurred_at"`
	AttemptID     string    `json:"attempt_id"`
	AnonymousID   string    `json:"anonymous_id"`
	Event         string    `json:"event"`
	CommandName   string    `json:"command_name"`
	CommandRoot   string    `json:"command_root,omitempty"`
	DurationMS    int64     `json:"duration_ms,omitempty"`
	Success       bool      `json:"success"`
	ErrorClass    string    `json:"error_class,omitempty"`
	CLIVersion    string    `json:"cli_version"`
	OS            string    `json:"os"`
	Arch          string    `json:"arch"`
	IsDevBuild    bool      `json:"is_dev_build"`
	// Properties carries an event's own dimensions beyond the core fields,
	// such as a deploy's phase timings. Callers pass only bounded numbers and
	// enums — never paths, hosts, names or error text.
	Properties map[string]string `json:"properties,omitempty"`
}

// coreProperties travel as top-level payload fields, not in Properties.
var coreProperties = map[string]bool{
	"command_name": true, "command_root": true, "duration_ms": true,
	"success": true, "error_class": true, "is_dev_build": true, "attempt_id": true,
}

const (
	// maxEventProperties and maxPropertyLen keep an event far below the
	// receiver's 8 KB body limit. maxPropertyLen is measured in bytes.
	maxEventProperties = 32
	maxPropertyLen     = 64
)

// extraProperties returns only documented safe dimensions, bounded in count
// and length, with a 128-byte allowance for canned remediation. Unknown keys and invalid enum/number values are discarded.
func extraProperties(properties map[string]string) map[string]string {
	keys := make([]string, 0, len(properties))
	for k := range properties {
		if !coreProperties[k] && allowedProperty(k, properties[k]) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	out := make(map[string]string, min(len(keys), maxEventProperties))
	for _, k := range keys[:min(len(keys), maxEventProperties)] {
		v := properties[k]
		out[k] = v
	}
	return out
}

var (
	stateMu    sync.Mutex
	client     *http.Client // nil when disabled
	enabled    bool
	distinctID string
	inFlight   = make(map[chan struct{}]struct{})
	// telemetryEndpoint is a variable so delivery behavior can be exercised
	// against an httptest server without sending test events to production.
	telemetryEndpoint = defaultTelemetryEndpoint

	// trackHook is set by tests to intercept events before they would be
	// sent to the telemetry endpoint. It is never set in production code.
	trackHook func(event string, properties map[string]string)
)

func SetTrackHookForTesting(fn func(event string, properties map[string]string)) {
	stateMu.Lock()
	defer stateMu.Unlock()
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
	}

	if cfg.Analytics != nil && !cfg.Analytics.Enabled {
		Disable()
		return firstRun
	}

	id, err := loadOrCreateID()
	if err != nil {
		Disable()
		return firstRun
	}

	stateMu.Lock()
	distinctID = id
	enabled = true
	client = &http.Client{Timeout: 5 * time.Second}
	stateMu.Unlock()
	return firstRun
}

// Track sends an analytics event. The HTTP send is a no-op when analytics is
// disabled or uninitialized; the test hook (if any) always fires so test
// assertions can observe the intended payload regardless of initialization state.
//
// The core properties (command_name, command_root, duration_ms, success,
// error_class, is_dev_build) become top-level payload fields. Every other
// property is sent in the payload's `properties` object, bounded in count and
// length, so callers must pass only bounded numbers and enums: never paths,
// hosts, names or error text.
func Track(event string, properties map[string]string) {
	track(event, properties, nil)
}

// track sends an event and calls onDelivered only after the receiver returns a
// 2xx response. Delivery remains best-effort: failures never affect the command
// result, but callers such as one-time milestones can defer local acknowledgement
// until the receiver has persisted the event.
func track(event string, properties map[string]string, onDelivered func()) {
	stateMu.Lock()
	hook := trackHook
	stateMu.Unlock()
	if hook != nil {
		hook(event, properties)
	}
	stateMu.Lock()
	if !enabled || client == nil {
		stateMu.Unlock()
		return
	}
	c, endpoint, id := client, telemetryEndpoint, distinctID
	done := make(chan struct{})
	inFlight[done] = struct{}{}
	stateMu.Unlock()

	payload := eventPayload{
		SchemaVersion: 2, EventID: uuid.NewString(), OccurredAt: time.Now().UTC(),
		AttemptID:   validAttemptID(properties["attempt_id"]),
		AnonymousID: id,
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
		Properties:  extraProperties(properties),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		finishDelivery(done)
		return
	}

	go func() {
		defer finishDelivery(done)
		resp, err := c.Post(endpoint, "application/json", bytes.NewReader(body))
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

func finishDelivery(done chan struct{}) {
	stateMu.Lock()
	delete(inFlight, done)
	close(done)
	stateMu.Unlock()
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
	track(name, map[string]string{"command_name": name, "success": "true"}, func() {
		_ = recordMilestone(dir, name)
	})
}

// TrackMilestoneOnce emits the named milestone event exactly once per
// config directory. It is a no-op when analytics is disabled. Milestone state
// stays in the config dir even when the identity is shared across configs.
func TrackMilestoneOnce(name string) {
	if !trackingEnabled() {
		return
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return
	}
	TrackMilestoneOnceInDir(dir, name)
}

// Flush waits for events admitted before this call without disabling later
// tracking. Call it before replacing the process at an elevation handoff.
func Flush() {
	waitForDeliveries(pendingDeliveries(false, false))
}

// Close stops admitting new events and waits for in-flight events to finish.
func Close() {
	waitForDeliveries(pendingDeliveries(true, false))
}

// Disable turns off analytics for the current process and flushes any pending events.
func Disable() {
	waitForDeliveries(pendingDeliveries(true, true))
}

// pendingDeliveries snapshots admission and outstanding work under one lock.
// Waiting holds no lock, so delivery callbacks can track additional events.
func pendingDeliveries(closeClient, disable bool) []chan struct{} {
	stateMu.Lock()
	defer stateMu.Unlock()
	if closeClient {
		client = nil
	}
	if disable {
		enabled = false
	}
	pending := make([]chan struct{}, 0, len(inFlight))
	for done := range inFlight {
		pending = append(pending, done)
	}
	return pending
}

func waitForDeliveries(pending []chan struct{}) {
	for _, done := range pending {
		<-done
	}
}

func trackingEnabled() bool {
	stateMu.Lock()
	defer stateMu.Unlock()
	return enabled && client != nil
}

// Enabled reports whether analytics is currently enabled.
func Enabled() bool {
	stateMu.Lock()
	defer stateMu.Unlock()
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
	// Identity belongs to the OS user, not a device configuration. MCP launchers
	// can override WENDY_CONFIG_DIR without becoming a different anonymous user.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determining analytics home directory: %w", err)
	}
	dir := filepath.Join(home, ".wendy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating analytics directory: %w", err)
	}

	idPath := filepath.Join(dir, "analytics_id")
	if id, err := readAnalyticsID(idPath); err == nil && id != "" {
		return id, nil
	}

	// Concurrent first launches must share one identity. Readers of an existing
	// ID need no writable lock file. Recheck failed reads after the writer exits:
	// Windows can report a sharing violation while it renames the new ID.
	release, err := flock.Acquire(filepath.Join(dir, "analytics_id.lock"), 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("locking analytics ID: %w", err)
	}
	defer release()
	if id, err := readAnalyticsID(idPath); err != nil || id != "" {
		return id, err
	}

	// Keep an existing shared identity authoritative. On the first launch after
	// upgrading, preserve the active config's old identity if the home has none.
	// Hold the shared lock through migration so different configs cannot race to
	// replace the shared ID. Leave the old file for older CLI versions.
	var id string
	if legacyDir := os.Getenv("WENDY_CONFIG_DIR"); filepath.IsAbs(legacyDir) && filepath.Clean(legacyDir) != dir {
		id, err = readAnalyticsID(filepath.Join(legacyDir, "analytics_id"))
		if err != nil {
			return "", err
		}
	}
	if id == "" {
		id = uuid.NewString()
	}
	if err := atomicfile.WritePreservingOwner(idPath, []byte(id), 0o600); err != nil {
		return "", fmt.Errorf("writing analytics ID: %w", err)
	}
	return id, nil
}

func readAnalyticsID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading analytics ID: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
