//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestTourRecoverySkipsExternalDriveWizard(t *testing.T) {
	for _, board := range []string{orinDeviceType, orinNanoDeviceType, thorDeviceType} {
		m := newTourWizardModel()
		m.selected = &deviceInfo{Key: board, LatestVersion: "1.2.3"}
		m.phase = phaseOSInstalled
		m.menuCursor = 1
		m, _ = stepTour(t, m, key("enter"))
		if m.phase != phaseNativeInstall {
			t.Fatalf("%s entered phase %v", board, m.phase)
		}
		args := tourNativeInstallArgs(m.selected)
		if slices.Contains(args, "--drive") || slices.Contains(args, "--force") {
			t.Fatalf("unsafe duplicated install args: %v", args)
		}
		if !slices.Contains(args, board) {
			t.Fatalf("wrong board: %v", args)
		}
		if isT234RecoveryDevice(board) {
			m.selected.Manifest = &deviceManifest{Versions: map[string]deviceVersion{"1.2.3": {InstallMode: "recovery"}}}
			if args = tourNativeInstallArgs(m.selected); !slices.Contains(args, "--rootfs-only=false") {
				t.Fatalf("recovery firmware not selected: %v", args)
			}
		}
	}
}

func TestTourQuitAndFailureAreNotCompletion(t *testing.T) {
	err := errors.New("failed flash")
	m := newTourWizardModel()
	m.phase = phaseError
	m.err = err
	if !errors.Is(tourModelResult(m), err) {
		t.Fatal("failure lost")
	}
	m.err = nil
	m.phase = phaseDiscovering
	if !errors.Is(tourModelResult(m), ErrUserCancelled) {
		t.Fatal("quit was reported as completion")
	}
	_, quit := stepTour(t, m, key("q"))
	if quit == nil {
		t.Fatal("advertised q does not quit")
	}
	m.phase = phaseCloud
	if tourModelResult(m) != nil {
		t.Fatal("completion rejected")
	}
}

func TestTourDiscoveryHasDeadline(t *testing.T) {
	m := newTourWizardModel()
	m.targetName = "expected"
	m.discoveryStarted = time.Now().Add(-3 * time.Minute)
	msg := m.cmdDiscoveryCheck()()
	if failed, ok := msg.(tourDiscoveryErrorMsg); !ok || failed.err == nil {
		t.Fatalf("did not stop discovery: %#v", msg)
	}
}

func TestTourDiscoveryDeadlineStartsAfterUSBPreflight(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseBootInstructions
	m.discoveryStarted = time.Now().Add(-3 * time.Minute)
	m, cmd := stepTour(t, m, key("enter"))
	if m.phase != phaseDiscovering || cmd == nil {
		t.Fatalf("did not begin USB preflight: phase=%v command=%v", m.phase, cmd != nil)
	}
	if !m.discoveryStarted.IsZero() {
		t.Fatal("discovery timeout includes USB setup and sudo prompts")
	}

	// Finishing the terminal preflight starts the full discovery allowance.
	before := time.Now()
	m, cmd = stepTour(t, m, tourDiscoveryTickMsg{})
	if m.discoveryStarted.Before(before) || m.discoveryStarted.After(time.Now()) || cmd == nil {
		t.Fatalf("first discovery tick did not start its deadline: %v", m.discoveryStarted)
	}

	// Later ticks retain that start time, so retries still expire.
	started := time.Now().Add(-3 * time.Minute)
	m.discoveryStarted = started
	m, cmd = stepTour(t, m, tourDiscoveryTickMsg{})
	if !m.discoveryStarted.Equal(started) || cmd == nil {
		t.Fatal("discovery retry reset its deadline")
	}
	if failed, ok := cmd().(tourDiscoveryErrorMsg); !ok || failed.err == nil {
		t.Fatal("discovery retry did not honor its original deadline")
	}
}

func TestTourSampleReadinessRequiresGeneratedStarterResponse(t *testing.T) {
	_, response, ok := strings.Cut(tourAppPy, "f\"\"\"")
	if !ok {
		t.Fatal("starter HTTP response template not found")
	}
	response, _, ok = strings.Cut(response, "\"\"\".encode()")
	if !ok {
		t.Fatal("starter HTTP response template end not found")
	}
	response = strings.NewReplacer("{hostname}", "test-edge", "{{", "{", "}}", "}").Replace(response)
	for _, tc := range []struct {
		name   string
		status int
		body   io.Reader
		want   bool
	}{
		{name: "generated starter", status: http.StatusOK, body: strings.NewReader(response), want: true},
		{name: "unrelated HTTP 200", status: http.StatusOK, body: strings.NewReader("<html><body>Another service is running</body></html>")},
		{name: "error response", status: http.StatusServiceUnavailable, body: strings.NewReader(response)},
		{name: "truncated response", status: http.StatusOK, body: io.MultiReader(strings.NewReader(response), iotest.ErrReader(io.ErrUnexpectedEOF))},
		{name: "oversized response", status: http.StatusOK, body: strings.NewReader(response + strings.Repeat("x", tourSampleResponseLimit))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tourSampleResponseReady(tc.status, tc.body); got != tc.want {
				t.Fatalf("ready=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestTourSampleReadinessBoundsResponseRead(t *testing.T) {
	body := strings.NewReader(strings.Repeat("x", 2*tourSampleResponseLimit))
	if tourSampleResponseReady(http.StatusOK, body) {
		t.Fatal("unrelated body satisfied readiness")
	}
	if got := body.Len(); got != tourSampleResponseLimit-1 {
		t.Fatalf("unread bytes=%d, want %d", got, tourSampleResponseLimit-1)
	}
}

func TestTourSampleReadinessRetainsCancellation(t *testing.T) {
	m := newTourWizardModel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.ctx = ctx
	m.foundAddr = "127.0.0.1"
	result, ok := m.cmdSampleReadiness()().(tourSampleReadyMsg)
	if !ok || !errors.Is(result.err, ErrUserCancelled) {
		t.Fatalf("result=%#v, want cancellation", result)
	}
}
