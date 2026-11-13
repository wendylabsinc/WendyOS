//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/shared/cliupdate"
)

// Check the emitted command line itself, including ANSI and indentation, and
// its position after the complete box. Stripping the whole output first would
// hide a command that is still styled or soft-wrapped by the renderer.
func assertCommandBelowBox(t *testing.T, output, command string) {
	t.Helper()
	end := strings.LastIndex(output, "╯")
	if end < 0 {
		t.Fatal("styled box is missing")
	}
	if strings.Contains(ansi.Strip(output[:end]), command) {
		t.Fatalf("command is still inside the box: %q", command)
	}
	if !strings.Contains("\n"+output[end:]+"\n", "\n"+command+"\n") {
		t.Fatalf("complete plain command line missing below box: %q\n%s", command, output)
	}
}

func TestJetsonUSBCommandsBelowBox(t *testing.T) {
	for _, packaged := range []bool{true, false} {
		command := cliupdate.InstallCommand("linux")
		if packaged {
			command = "sudo usermod -aG plugdev $USER"
		}
		output := renderUSBAccessHint("linux", packaged)
		assertCommandBelowBox(t, output, command)
		box := output[:strings.LastIndex(output, "╯")]
		if !strings.Contains(box, "command below") || !strings.Contains(box, "with sudo") {
			t.Fatalf("missing in-box remedy context: %s", box)
		}
	}
}

func TestDragonwingUSBCommandsBelowBox(t *testing.T) {
	for _, packaged := range []bool{true, false} {
		command := cliupdate.InstallCommand("linux")
		if packaged {
			command = "sudo usermod -aG plugdev $USER"
		}
		assertCommandBelowBox(t, renderDragonwingUSBAccessHint("linux", packaged), command)
	}
	for _, goos := range []string{"darwin", "windows"} {
		output := renderDragonwingUSBAccessHint(goos, false)
		if strings.Contains(output, "curl ") || strings.Contains(output, "usermod ") {
			t.Fatalf("Linux remedy on %s: %s", goos, output)
		}
	}
}

func TestThorRecoveryCommandsBelowBox(t *testing.T) {
	var output strings.Builder
	printThorBadStateHint(&output)
	for _, slot := range []string{"A", "B"} {
		assertCommandBelowBox(t, output.String(), "setvar RootfsStatusSlot"+slot+" -guid 781E084C-A330-417C-B678-38E696380CB9 -bs -rt -nv =0x00000000")
	}
}

func TestOrinRetryCommandsBelowBox(t *testing.T) {
	for _, board := range []string{orinDeviceType, orinNanoDeviceType} {
		for _, storage := range []string{"nvme", "sd"} {
			var output strings.Builder
			printOrinBadStateHint(&output, t234InstallOptions{DeviceType: board, Storage: storage})
			assertCommandBelowBox(t, output.String(), "wendy os install --device-type "+board+" --storage "+storage)
		}
	}
}

func TestTourWelcomeCommandBelowBox(t *testing.T) {
	for _, width := range []int{40, 100} {
		m := newTourWizardModel()
		m.phase, m.width = phaseWelcome, width
		assertCommandBelowBox(t, m.View(), "wendy tour")
	}
}

func TestTourLinuxInstallCommandsBelowBox(t *testing.T) {
	commands := []string{
		linuxDesktopCommand("", "", linuxDesktopMachineLabel),
		"curl -fsSL https://install.wendy.dev/apt-key.gpg | sudo apt-key add -",
		`echo "deb https://apt.wendy.dev stable main" | sudo tee /etc/apt/sources.list.d/wendy.list`,
		"sudo apt update && sudo apt install -y wendy-agent",
	}
	for _, width := range []int{40, 100} {
		m := newTourWizardModel()
		m.phase, m.width = phaseAptInstall, width
		output, previous := m.View(), -1
		for _, command := range commands {
			assertCommandBelowBox(t, output, command)
			index := strings.Index(output, "\n"+command+"\n")
			if index <= previous {
				t.Fatal("Linux install command order changed")
			}
			previous = index
		}
	}
}

func TestTourCompletionCommandsBelowBox(t *testing.T) {
	for _, width := range []int{40, 100} {
		for _, project := range []string{"", "/tmp/sample 'quoted'"} {
			m := newTourWizardModel()
			m.phase, m.width, m.projectPath, m.foundAddr = phaseCloud, width, project, "sample.local"
			output := m.View()
			assertCommandBelowBox(t, output, "wendy auth login")
			if project != "" {
				assertCommandBelowBox(t, output, tourSampleRedeployCommand(runtime.GOOS, project, m.foundAddr))
			} else if strings.Contains(output, "wendy run ") {
				t.Fatal("skipped sample gained a redeploy command")
			}
		}
	}
}

func TestTourErrorDiagnosticsAndCommandsBelowBox(t *testing.T) {
	for _, width := range []int{40, 100} {
		m := newTourWizardModel()
		m.targetName, m.discoveryStarted = "edge", time.Now().Add(-3*time.Minute)
		failure := m.cmdDiscoveryCheck()().(tourDiscoveryErrorMsg)
		m, _ = stepTour(t, m, failure)
		m.width = width
		output := m.View()
		verify := "wendy install verify --address edge.local"
		if runtime.GOOS == "windows" {
			verify = "wendy install verify --address 'edge.local'"
		}
		for _, command := range []string{"wendy discover", verify, "wendy tour --hardware", "wendy run"} {
			assertCommandBelowBox(t, output, command)
		}
		if !strings.Contains(output, `could not verify "edge" within two minutes`) || !strings.Contains(output, "accept USB setup") && !strings.Contains(output, "Accept USB setup") {
			t.Fatal("discovery failure context lost")
		}
		raw := "transport refused\nwendy custom --example 'literal argument'"
		m.err = errors.New(raw)
		output = m.View()
		if !strings.Contains(output[strings.LastIndex(output, "╯"):], raw) {
			t.Fatal("raw diagnostic evidence was wrapped, styled or lost")
		}
		assertCommandBelowBox(t, output, "wendy custom --example 'literal argument'")
	}
}

func TestTourReadinessLogsCommandBelowBox(t *testing.T) {
	m := newTourWizardModel()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	m.ctx, m.foundAddr, m.projectPath = ctx, "edge.local", "/tmp/sample 'quoted'"
	failure := m.cmdSampleReadiness()().(tourSampleReadyMsg)
	if !errors.Is(failure.err, context.DeadlineExceeded) {
		t.Fatalf("readiness cause lost: %v", failure.err)
	}
	m, _ = stepTour(t, m, failure)
	for _, width := range []int{40, 100} {
		m.width = width
		output := m.View()
		logs := "wendy device logs --device edge.local"
		if runtime.GOOS == "windows" {
			logs = "wendy device logs --device 'edge.local'"
		}
		assertCommandBelowBox(t, output, logs)
		if !strings.Contains(output, m.sampleURL()) || !strings.Contains(output, m.projectPath) || !strings.Contains(output, context.DeadlineExceeded.Error()) {
			t.Fatal("readiness evidence lost")
		}
	}
}

func TestTourDiscoverySuggestionPreservesPOSIXAddress(t *testing.T) {
	m := newTourWizardModel()
	m.hostname = "edge ' $wendy_test_quote $(printf expanded); #"
	m.discoveryStarted = time.Now().Add(-3 * time.Minute)
	failure := m.cmdDiscoveryCheck()().(tourDiscoveryErrorMsg)
	command := NextSteps(failure.err)[1]
	got := tourPrintedCommandArgs(t, command)
	want := []string{"install", "verify", "--address", m.hostname}
	if !slices.Equal(got, want) {
		t.Fatalf("suggestion changed address: got %q want %q", got, want)
	}
}

func TestTourDiscoverySuggestionUsesVerificationAddress(t *testing.T) {
	for _, tc := range []struct{ hostname, want string }{
		{"192.0.2.10", "192.0.2.10"},
		{"", "edge.local"},
	} {
		m := newTourWizardModel()
		m.targetName, m.hostname = "edge", tc.hostname
		m.discoveryStarted = time.Now().Add(-3 * time.Minute)
		failure := m.cmdDiscoveryCheck()().(tourDiscoveryErrorMsg)
		command := NextSteps(failure.err)[1]
		want := "wendy install verify --address " + tc.want
		if runtime.GOOS == "windows" {
			want = "wendy install verify --address '" + tc.want + "'"
		}
		if command != want {
			t.Fatalf("hostname %q: command %q, want %q", tc.hostname, command, want)
		}
	}
}
