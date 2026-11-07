package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

// StatusPath is where the supervisor tells the running agent its role.
const StatusPath = "/run/wendy-agent-supervisor/status.json"

type UpdateState string

const (
	UpdateNone     UpdateState = ""
	UpdateTrial    UpdateState = "trial"
	UpdateApproved UpdateState = "approved"
	UpdateRejected UpdateState = "rejected"
)

// Status is what the supervisor tells the running agent (and through it the
// CLI) about which agent runs and how the last update went.
type Status struct {
	Role         Role        `json:"role"`
	Version      string      `json:"version,omitempty"`
	SHA256       string      `json:"sha256,omitempty"`
	PID          int         `json:"pid,omitempty"`
	ImageVersion string      `json:"image_version,omitempty"`
	UpdateState  UpdateState `json:"update_state,omitempty"`
	Reason       string      `json:"reason,omitempty"`
	// UpdateSHA256 names the agent UpdateState is about, so a client can tell
	// its own upload's verdict from an earlier one.
	UpdateSHA256 string `json:"update_sha256,omitempty"`
	// Revalidation marks a trial that re-checks the approved agent after an
	// OS update; uploads and OS updates need not wait for it.
	Revalidation bool `json:"revalidation,omitempty"`
	// UpdatesPaused says why the supervisor cannot take an upload right now.
	UpdatesPaused string `json:"updates_paused,omitempty"`
	// Automatic marks a trial the daily update started.
	Automatic bool `json:"automatic,omitempty"`
	// Grace: the running agent passed its checks moments ago; if it keeps
	// crashing or stops answering in the next minutes, the device still goes back.
	Grace bool `json:"grace,omitempty"`
	// AgentStarted is the boot-clock time the running agent started.
	AgentStarted time.Duration `json:"agent_started,omitempty"`
}

func writeStatus(path string, st Status) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, data, 0o644)
}

// PausedReason is why the supervisor cannot take an upload now, or "".
func PausedReason(path string) string {
	st, err := ReadStatus(path)
	if err != nil {
		return ""
	}
	return st.UpdatesPaused
}

func ReadStatus(path string) (Status, error) {
	var st Status
	data, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(data, &st)
	return st, err
}
