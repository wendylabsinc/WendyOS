//go:build darwin && !ios

package commands

import (
	"fmt"
	"io"
	"testing"
)

func TestDesktopSimulatorControlsMatchRuntimeProtocols(t *testing.T) {
	for _, profile := range []string{"go2", "g1", "so101", "rosmaster-r2"} {
		for _, action := range []string{"pause", "resume", "reset"} {
			t.Run(profile+"/"+action, func(t *testing.T) {
				path, body := desktopSimulatorControl(profile, "/api/"+action)
				if (profile == "so101" || profile == "rosmaster-r2") && action != "reset" {
					if path != "/api/pause" || body == nil {
						t.Fatal("missing pause body")
					}
					data, err := io.ReadAll(body)
					if err != nil {
						t.Fatal(err)
					}
					want := `{"paused":true}`
					if action == "resume" {
						want = `{"paused":false}`
					}
					if string(data) != want {
						t.Fatalf("got %s, want %s", data, want)
					}
				} else if path != "/api/"+action || body != nil {
					t.Fatal("changed Unitree/reset protocol")
				}
			})
		}
	}
}

func TestDesktopSimulatorRecoveryUsesWrappedErrorTypes(t *testing.T) {
	if got := DesktopSimulatorRecovery(fmt.Errorf("start: %w", &robotSourceMismatchError{name: "g1", runtimeName: "G1"})); got != 1 {
		t.Fatalf("runtime recovery=%d", got)
	}
	if got := DesktopSimulatorRecovery(fmt.Errorf("start: %w", &robotAgentCapabilityError{name: "g1"})); got != 2 {
		t.Fatalf("agent recovery=%d", got)
	}
	if got := DesktopSimulatorRecovery(fmt.Errorf("pins a different runtime")); got != 0 {
		t.Fatal("inferred recovery from text")
	}
	if got := DesktopSimulatorRecovery(nil); got != 0 {
		t.Fatal("success has a recovery")
	}
}
