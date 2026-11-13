package analytics

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

const activationStateFile = "activation-state.json"

var activationStateMu sync.Mutex

// Only timestamps are persisted. No project, device, command arguments, or
// identity beyond the existing installation's config directory is recorded.
type activationState struct {
	InstalledAt time.Time `json:"installed_at"`
	Observed24h time.Time `json:"observed_24h,omitempty"`
	Observed7d  time.Time `json:"observed_7d,omitempty"`
}

type activationFollowup struct {
	event       string
	installedAt time.Time
	observedAt  time.Time
}

func trackActivationFollowups(ctx context.Context, event, command, outcome string) {
	if !trackingEnabled() || !activationStateEvent(event) {
		return
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return
	}
	for _, followup := range activationFollowups(dir, event, outcome, time.Now().UTC(), true) {
		trackStage(ctx, followup.event, command, "completed", "", map[string]string{"stage": "install"}, func() {
			acknowledgeActivationFollowup(dir, followup)
		})
	}
}

func activationStateEvent(event string) bool {
	return event == "cli_seen" || event == "install_finished" || event == "os_install_finished" || event == "run_started"
}

// Successful delivery records each threshold, so later launches retry observations
// that could not be delivered.
// A process-local lock and atomic replacement protect against partial writes;
// concurrent CLI processes can still observe the same threshold. These are
// next-launch observations, never evidence that an absent user abandoned setup.
func activationFollowups(dir, event, outcome string, now time.Time, trackingEnabled bool) []activationFollowup {
	if !trackingEnabled || !activationStateEvent(event) {
		return nil
	}
	activationStateMu.Lock()
	defer activationStateMu.Unlock()
	path := filepath.Join(dir, activationStateFile)
	if event == "run_started" {
		_ = os.Remove(path)
		return nil
	}
	var state activationState
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &state)
	}
	if event == "install_finished" || event == "os_install_finished" {
		if outcome == "completed" {
			writeActivationState(dir, activationState{InstalledAt: now})
		}
		return nil
	}
	if state.InstalledAt.IsZero() || now.Before(state.InstalledAt) {
		return nil
	}
	var events []activationFollowup
	if now.Sub(state.InstalledAt) >= 24*time.Hour && state.Observed24h.IsZero() {
		events = append(events, activationFollowup{"next_launch_no_run_24h", state.InstalledAt, now})
	}
	if now.Sub(state.InstalledAt) >= 7*24*time.Hour && state.Observed7d.IsZero() {
		events = append(events, activationFollowup{"next_launch_no_run_7d", state.InstalledAt, now})
	}
	return events
}

func acknowledgeActivationFollowup(dir string, followup activationFollowup) {
	activationStateMu.Lock()
	defer activationStateMu.Unlock()
	data, err := os.ReadFile(filepath.Join(dir, activationStateFile))
	if err != nil {
		return
	}
	var state activationState
	if json.Unmarshal(data, &state) != nil || !state.InstalledAt.Equal(followup.installedAt) {
		return
	}
	// An in-flight delivery must not recreate a window cleared by a run or
	// acknowledge a threshold belonging to a subsequent installation.
	switch followup.event {
	case "next_launch_no_run_24h":
		if !state.Observed24h.IsZero() {
			return
		}
		state.Observed24h = followup.observedAt
	case "next_launch_no_run_7d":
		if !state.Observed7d.IsZero() {
			return
		}
		state.Observed7d = followup.observedAt
	default:
		return
	}
	writeActivationState(dir, state)
}

func writeActivationState(dir string, state activationState) {
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	_ = atomicfile.Write(filepath.Join(dir, activationStateFile), data, 0600)
}
