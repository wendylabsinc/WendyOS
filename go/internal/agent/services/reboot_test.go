package services

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// recordingRebooter builds a rebooter whose every side effect appends to an
// ordered event log, so tests can assert not just *that* the flush happened but
// that it happened *before* the restart — the whole point of WDY-2200.
func recordingRebooter(events *[]string, cleanErr error) rebooter {
	return rebooter{
		logger: zap.NewNop(),
		sync:   func() { *events = append(*events, "sync") },
		immediate: func() error {
			*events = append(*events, "immediate")
			return nil
		},
		clean: func() error {
			*events = append(*events, "clean")
			return cleanErr
		},
		grace: 60 * time.Second,
		sleep: func(d time.Duration) { *events = append(*events, fmt.Sprintf("sleep(%s)", d)) },
	}
}

// A reboot that does not flush first can lose any recently written data. On
// WendyOS < 0.18.1 the data it loses is the staged UEFI capsule, which strands
// the device's OTA entirely.
func TestRebootFlushesBeforeRestarting(t *testing.T) {
	var events []string
	if err := recordingRebooter(&events, nil).reboot(); err != nil {
		t.Fatalf("reboot() = %v, want nil", err)
	}
	if got := strings.Join(events, ","); got != "sync,immediate" {
		t.Errorf("events = %q, want %q", got, "sync,immediate")
	}
}

// The clean path must flush AND hand over to systemd, whose unmount is what
// WDY-2200 validated on hardware. The forced restart must only happen after the
// grace period, never instead of the clean attempt.
func TestRebootCleanFlushesThenHandsOverToSystemd(t *testing.T) {
	var events []string
	if err := recordingRebooter(&events, nil).rebootClean(); err != nil {
		t.Fatalf("rebootClean() = %v, want nil", err)
	}
	want := "sync,clean,sleep(1m0s),immediate"
	if got := strings.Join(events, ","); got != want {
		t.Errorf("events = %q, want %q", got, want)
	}
}

// A clean shutdown can hang on a stuck unmount or a container that will not
// stop. Before this change the reboot was instantaneous, so the clean path adds
// a new way to wedge a device mid-update; the watchdog is what bounds it.
func TestRebootCleanForcesRestartAfterTheGracePeriod(t *testing.T) {
	var events []string
	r := recordingRebooter(&events, nil)
	var slept time.Duration
	r.sleep = func(d time.Duration) { slept = d; events = append(events, "sleep") }
	if err := r.rebootClean(); err != nil {
		t.Fatalf("rebootClean() = %v, want nil", err)
	}
	if slept != 60*time.Second {
		t.Errorf("grace period = %v, want 60s", slept)
	}
	if got := strings.Join(events, ","); !strings.HasSuffix(got, "sleep,immediate") {
		t.Errorf("events = %q, want the forced restart to come after the sleep", got)
	}
}

// If systemd cannot be reached at all there is nothing to wait for, so the
// fallback must be immediate rather than idling for the full grace period.
func TestRebootCleanFallsBackWithoutWaitingWhenSystemdFails(t *testing.T) {
	var events []string
	if err := recordingRebooter(&events, errors.New("systemctl: not found")).rebootClean(); err != nil {
		t.Fatalf("rebootClean() = %v, want nil", err)
	}
	want := "sync,clean,immediate"
	if got := strings.Join(events, ","); got != want {
		t.Errorf("events = %q, want %q", got, want)
	}
}

// The version gate: an affected OS must take the systemd path, and a current OS
// must not. A current OS taking a different reboot path than it was tested with
// would be a silent behaviour change for no benefit.
func TestRebootAfterOSUpdatePicksThePathTheOSNeeds(t *testing.T) {
	tests := []struct {
		name      string
		osVersion string
		want      string
	}{
		{"0.17.0 needs the systemd handover", "WendyOS-0.17.0", "sync,clean,sleep(1m0s),immediate"},
		{"0.18.0 needs the systemd handover", "0.18.0", "sync,clean,sleep(1m0s),immediate"},
		{"0.18.1 restarts immediately after a flush", "WendyOS-0.18.1", "sync,immediate"},
		{"0.18.2 restarts immediately after a flush", "WendyOS-0.18.2", "sync,immediate"},
		{"a dev build restarts immediately after a flush", "dev", "sync,immediate"},
		{"an unknown version restarts immediately after a flush", "", "sync,immediate"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			if err := rebootAfterOSUpdate(recordingRebooter(&events, nil), tc.osVersion); err != nil {
				t.Fatalf("rebootAfterOSUpdate() = %v, want nil", err)
			}
			if got := strings.Join(events, ","); got != tc.want {
				t.Errorf("events = %q, want %q", got, tc.want)
			}
		})
	}
}

// The argument must reach systemd's final reboot(2): "forced-recovery" is what
// puts a Jetson into USB recovery mode, and without it the device boots normally.
func TestSystemctlRebootArgs(t *testing.T) {
	tests := []struct {
		argument string
		want     string
	}{
		{"", "--no-block reboot"},
		{"forced-recovery", "--no-block --reboot-argument=forced-recovery reboot"},
	}
	for _, test := range tests {
		if got := strings.Join(systemctlRebootArgs(test.argument), " "); got != test.want {
			t.Errorf("systemctlRebootArgs(%q) = %q, want %q", test.argument, got, test.want)
		}
	}
}
