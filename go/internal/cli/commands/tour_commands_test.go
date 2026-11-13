//go:build darwin || linux || windows

package commands

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Run printed commands with a shell function in place of Wendy to observe the
// arguments a copy-paste produces, including any accidental shell expansion.
func tourPrintedCommandArgs(t *testing.T, command string) []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell quoting is for macOS and Linux")
	}
	t.Setenv("WENDY_TEST_QUOTE", "expanded")
	output, err := exec.Command("sh", "-c", "wendy() { printf '%s\\0' \"$@\"; }\n"+command).CombinedOutput()
	if err != nil {
		t.Fatalf("run printed command: %v\n%s", err, output)
	}
	return strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
}

func TestTourSampleRedeployCommandPreservesPOSIXArguments(t *testing.T) {
	m := newTourWizardModel()
	m.projectPath = "/tmp/starter ' \" $WENDY_TEST_QUOTE $(printf expanded) `printf expanded`; #"
	m.foundAddr = "device ' $WENDY_TEST_QUOTE $(printf expanded) `printf expanded`; #"
	view := ansi.Strip(m.viewCloud(4096))
	var command string
	for _, line := range strings.Split(view, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "wendy run ") {
			command = strings.TrimSpace(line)
			break
		}
	}
	if command == "" {
		t.Fatal("completion view has no redeploy command")
	}
	got := tourPrintedCommandArgs(t, command)
	want := []string{"run", "--prefix", m.projectPath, "--device", m.foundAddr, "--detach"}
	if !slices.Equal(got, want) {
		t.Fatalf("printed command changed arguments:\n got %q\nwant %q", got, want)
	}
}

func TestTourSampleRedeployCommandUsesPowerShellLiteralsOnWindows(t *testing.T) {
	got := tourSampleRedeployCommand("windows", `C:\Users\O'Brien\$starter & tour`, "O'Brien $device; `literal`")
	want := "wendy run --prefix 'C:\\Users\\O''Brien\\$starter & tour' --device 'O''Brien $device; `literal`' --detach"
	if got != want {
		t.Fatalf("PowerShell command:\n got %s\nwant %s", got, want)
	}
}

func TestTourRobotResumeCommandPreservesPOSIXArguments(t *testing.T) {
	dir := "/tmp/starter ' \" $WENDY_TEST_QUOTE $(printf expanded) `printf expanded`; #"
	command := tourRobotResumeCommand(filepath.Join(dir, "tour.json"))
	got := tourPrintedCommandArgs(t, command)
	want := []string{"tour", "--simulator", "--project", dir}
	if !slices.Equal(got, want) {
		t.Fatalf("printed command changed arguments:\n got %q\nwant %q", got, want)
	}
}
