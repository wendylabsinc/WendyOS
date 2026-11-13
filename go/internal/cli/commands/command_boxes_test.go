package commands

import (
	"strings"
	"testing"

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
