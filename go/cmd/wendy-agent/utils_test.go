package main

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/oshealth"
)

func TestUnknownUtilityCommandExitsInsteadOfStartingTheAgent(t *testing.T) {
	handled, code := handleUtilityCommand([]string{"utils", "no-such-command"})
	if !handled || code != 2 {
		t.Fatalf("an unknown utils command must exit 2, got handled=%v code=%d", handled, code)
	}
}

func TestOSGateUtilityRunsTheGate(t *testing.T) {
	ran := false
	orig := runOSGateUtility
	runOSGateUtility = func() int { ran = true; return 0 }
	t.Cleanup(func() { runOSGateUtility = orig })

	handled, code := handleUtilityCommand([]string{"utils", "os-gate"})
	if !handled || code != 0 || !ran {
		t.Fatalf("utils os-gate: handled=%v code=%d ran=%v", handled, code, ran)
	}
}

func TestOSGateIsSkippedWhenTheSupervisorRanIt(t *testing.T) {
	t.Setenv("WENDY_OS_GATE", "")
	if osGateDoneBySupervisor() {
		t.Fatal("without the supervisor the agent runs the gate itself")
	}
	t.Setenv("WENDY_OS_GATE", "done")
	if !osGateDoneBySupervisor() {
		t.Fatal("the supervisor already ran the gate")
	}
}

func TestOSGateUtilityFailsWhileTheUpdateStillWaitsForItsCommit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker *oshealth.PendingMarker
		want   int
	}{
		{"nothing pending", nil, 0},
		{"commit kept for a retry", &oshealth.PendingMarker{BootID: "the boot that installed it"}, 1},
		{"not rebooted yet", &oshealth.PendingMarker{BootID: oshealth.CurrentBootID()}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.marker != nil {
				tc.marker.CreatedAt = time.Now()
				if err := oshealth.WritePendingMarker(dir, *tc.marker); err != nil {
					t.Fatal(err)
				}
			}
			if got := osGateExitCode(dir); got != tc.want {
				t.Fatalf("exit code %d, want %d", got, tc.want)
			}
		})
	}
}
