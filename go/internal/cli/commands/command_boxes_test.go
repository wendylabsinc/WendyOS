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
