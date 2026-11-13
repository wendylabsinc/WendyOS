//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestTourDeclinedChildInstallIsCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses the POSIX shell to provide a real child exit status")
	}
	err := exec.Command("sh", "-c", "exit 130").Run()
	if err == nil {
		t.Fatal("expected child exit error")
	}
	m := newTourWizardModel()
	m.phase = phaseInstalling
	m, _ = stepTour(t, m, tourOSInstallDoneMsg{native: true, err: tourChildError(m.ctx, err)})
	if m.installedByTour || m.phase != phaseError || !errors.Is(tourModelResult(m), ErrUserCancelled) {
		t.Fatalf("declined flash advanced as success: %+v", m)
	}
	if tourOutcome(m.err) != "cancelled" {
		t.Fatal("cancellation counted as product failure")
	}
	failure := exec.Command("sh", "-c", "exit 7").Run()
	if got := tourChildError(context.Background(), failure); got != failure {
		t.Fatal("ordinary child failure was replaced")
	}
	if got := tourChildError(context.Background(), nil); got != nil {
		t.Fatal("successful child became a failure")
	}
}

func TestTourCancelledContextOverridesKilledChildError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(tourChildError(ctx, errors.New("child killed")), ErrUserCancelled) {
		t.Fatal("context cancellation lost")
	}
}

func TestTourFailureKeepsActualPhaseAcrossErrorRedraws(t *testing.T) {
	for _, test := range []struct {
		phase tourPhase
		msg   tea.Msg
		want  string
	}{
		{phaseLoadDevices, tourDevicesLoadedMsg{err: errors.New("download")}, "select_target"},
		{phaseInstalling, tourOSInstallDoneMsg{err: errors.New("flash")}, "provision"},
		{phaseDiscovering, tourDiscoveryErrorMsg{err: context.DeadlineExceeded}, "first_boot"},
		{phaseTemplateLoading, tourSampleCreatedMsg{err: errors.New("file")}, "project"},
		{phaseRunProject, tourRunDoneMsg{err: errors.New("build")}, "deploy"},
		{phaseSampleReadiness, tourSampleReadyMsg{err: context.DeadlineExceeded}, "readiness"},
	} {
		m := newTourWizardModel()
		m.phase = test.phase
		m, _ = stepTour(t, m, test.msg)
		m, _ = stepTour(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
		m, _ = stepTour(t, m, key("enter"))
		if m.phase != phaseError || m.diagnosticStep != test.want {
			t.Errorf("phase %v lost failure step: got %q want %q", test.phase, m.diagnosticStep, test.want)
		}
	}
}

func TestTourRunTransitionRecordsDeployPhase(t *testing.T) {
	m := newTourWizardModel()
	m.phase = phaseCreateProject
	m.projectPath = t.TempDir()
	m.foundAddr = "127.0.0.1"
	m, command := stepTour(t, m, key("enter"))
	if command == nil || m.phase != phaseRunProject {
		t.Fatal("deployment still reports project preparation")
	}
	// Do not execute the child. Deliver its failure through the normal state machine.
	m, _ = stepTour(t, m, tourRunDoneMsg{err: errors.New("deployment failed")})
	if m.diagnosticStep != "deploy" {
		t.Fatalf("step=%q", m.diagnosticStep)
	}
}

func TestTourSampleReadinessPreservesCancellationAndDeadline(t *testing.T) {
	for _, cancelled := range []bool{true, false} {
		m := newTourWizardModel()
		m.foundAddr = "127.0.0.1"
		var cancel context.CancelFunc
		if cancelled {
			m.ctx, cancel = context.WithCancel(context.Background())
		} else {
			m.ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}
		cancel()
		msg, ok := m.cmdSampleReadiness()().(tourSampleReadyMsg)
		if !ok {
			t.Fatal("unexpected readiness result")
		}
		want := error(context.DeadlineExceeded)
		if cancelled {
			want = ErrUserCancelled
		}
		if !errors.Is(msg.err, want) {
			t.Fatalf("cancelled=%v: got %v, want %v", cancelled, msg.err, want)
		}
	}
}
