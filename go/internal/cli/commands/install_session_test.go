//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// stubSessionTerminals fakes what startInstallSession probes and mirrors root's
// --json auto-switch; a nil terminal means none to draw prompts on.
func stubSessionTerminals(t *testing.T, bothTTY, stdinTTY bool, terminal *os.File) (probes, closes *int) {
	t.Helper()
	origInteractive, origHuman, origProbe := isInteractiveTerminalFn, humanPresentFn, promptTerminalFn
	origJSON, origOrg := jsonOutput, canPromptForOrgFn
	t.Cleanup(func() {
		isInteractiveTerminalFn, humanPresentFn, promptTerminalFn = origInteractive, origHuman, origProbe
		jsonOutput, canPromptForOrgFn = origJSON, origOrg
	})
	isInteractiveTerminalFn = func() bool { return bothTTY }
	humanPresentFn = func() bool { return stdinTTY }
	jsonOutput = !bothTTY
	canPromptForOrgFn = func() bool { return !jsonOutput }
	probes, closes = new(int), new(int)
	promptTerminalFn = func() (*os.File, func(), bool) {
		*probes++
		if terminal == nil {
			return nil, nil, false
		}
		return terminal, func() { *closes++ }, true
	}
	return probes, closes
}

// sessionCmd returns an install command under a root that owns --json, set
// explicitly when explicitJSON is true.
func sessionCmd(t *testing.T, explicitJSON bool) *cobra.Command {
	t.Helper()
	var flag bool
	root := &cobra.Command{Use: "wendy"}
	root.PersistentFlags().BoolVar(&flag, "json", false, "")
	install := &cobra.Command{Use: "install"}
	root.AddCommand(install)
	if explicitJSON {
		if err := root.PersistentFlags().Set("json", "true"); err != nil {
			t.Fatal(err)
		}
	}
	return install
}

func fakeTerminal(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestInstallSessionPipedStdoutDrawsPromptsOnTerminal(t *testing.T) {
	terminal := fakeTerminal(t)
	probes, closes := stubSessionTerminals(t, false, true, terminal)

	end := startInstallSession(sessionCmd(t, false))
	if !isInteractiveTerminal() {
		t.Fatal("stdin at a terminal with stdout piped should be interactive")
	}
	if w, ok := tui.PromptOutput(); !ok || w != terminal {
		t.Fatalf("prompt output = %v, %v; want the terminal", w, ok)
	}
	if jsonOutput {
		t.Fatal("JSON output stayed on although only a pipe turned it on")
	}
	if !canPromptForOrgFn() {
		t.Fatal("org picker refused in an interactive install")
	}
	end()

	if *probes != 1 || *closes != 1 {
		t.Fatalf("terminal probed %d times, closed %d times; want 1 and 1", *probes, *closes)
	}
	if isInteractiveTerminal() {
		t.Fatal("interactivity not restored after the session")
	}
	if _, ok := tui.PromptOutput(); ok {
		t.Fatal("prompt output still set after the session")
	}
	if !jsonOutput {
		t.Fatal("JSON output not restored after the session")
	}
}

func TestInstallSessionBothTerminalsUnchanged(t *testing.T) {
	probes, closes := stubSessionTerminals(t, true, true, fakeTerminal(t))

	end := startInstallSession(sessionCmd(t, false))
	defer end()
	if !isInteractiveTerminal() {
		t.Fatal("a full terminal session should be interactive")
	}
	if _, ok := tui.PromptOutput(); ok {
		t.Fatal("prompt output set although stdout is a terminal")
	}
	if *probes != 0 || *closes != 0 {
		t.Fatalf("terminal probed %d times; want none", *probes)
	}
	if jsonOutput {
		t.Fatal("JSON output turned on in a terminal session")
	}
}

func TestInstallSessionWithoutTerminalIsNonInteractive(t *testing.T) {
	probes, _ := stubSessionTerminals(t, false, false, fakeTerminal(t))

	end := startInstallSession(sessionCmd(t, false))
	defer end()
	if isInteractiveTerminal() {
		t.Fatal("no terminal on stdin should be non-interactive")
	}
	if _, ok := tui.PromptOutput(); ok {
		t.Fatal("prompt output set without a person at a terminal")
	}
	if *probes != 0 {
		t.Fatal("probed for a terminal although stdin is not one")
	}
	if !jsonOutput {
		t.Fatal("JSON auto-switch dropped in a non-interactive run")
	}
	if canPromptForOrgFn() {
		t.Fatal("org picker allowed in a non-interactive install")
	}
}

func TestInstallSessionNoTerminalToDrawOn(t *testing.T) {
	probes, _ := stubSessionTerminals(t, false, true, nil)

	end := startInstallSession(sessionCmd(t, false))
	defer end()
	if *probes != 1 {
		t.Fatalf("terminal probed %d times; want 1", *probes)
	}
	if isInteractiveTerminal() {
		t.Fatal("interactive although no terminal could show the prompts")
	}
}

func TestInstallSessionKeepsExplicitJSON(t *testing.T) {
	terminal := fakeTerminal(t)
	stubSessionTerminals(t, false, true, terminal)
	jsonOutput = true // --json, as root parsed it

	end := startInstallSession(sessionCmd(t, true))
	defer end()
	if !isInteractiveTerminal() {
		t.Fatal("explicit --json should not take the terminal away from prompts")
	}
	if w, ok := tui.PromptOutput(); !ok || w != terminal {
		t.Fatalf("prompt output = %v, %v; want the terminal", w, ok)
	}
	if !jsonOutput {
		t.Fatal("explicit --json was turned off")
	}
	if canPromptForOrgFn() {
		t.Fatal("org picker allowed under explicit --json")
	}
}

func TestInstallSessionDecidesOnce(t *testing.T) {
	stubSessionTerminals(t, false, true, fakeTerminal(t))
	calls := 0
	isInteractiveTerminalFn = func() bool { calls++; return false }

	end := startInstallSession(sessionCmd(t, false))
	defer end()
	humanPresentFn = func() bool { return false }
	for i := 0; i < 3; i++ {
		if !isInteractiveTerminal() {
			t.Fatal("the decision changed during the run")
		}
	}
	if calls != 1 {
		t.Fatalf("terminal state read %d times; want once", calls)
	}
}

func TestInstallSessionRestoresAfterPanic(t *testing.T) {
	stubSessionTerminals(t, false, true, fakeTerminal(t))

	func() {
		defer func() { _ = recover() }()
		defer startInstallSession(sessionCmd(t, false))()
		panic("install blew up")
	}()

	if isInteractiveTerminal() {
		t.Fatal("interactivity not restored after a panic")
	}
	if _, ok := tui.PromptOutput(); ok {
		t.Fatal("prompt output not restored after a panic")
	}
	if !jsonOutput {
		t.Fatal("JSON output not restored after a panic")
	}
}

func TestInstallCommandRunsInOneSession(t *testing.T) {
	terminal := fakeTerminal(t)
	_, closes := stubSessionTerminals(t, false, true, terminal)
	origFetch := fetchInstallMainManifest
	t.Cleanup(func() { fetchInstallMainManifest = origFetch })
	var sawInteractive bool
	var sawOutput io.Writer
	fetchInstallMainManifest = func(context.Context, int) (*mainManifest, error) {
		sawInteractive = isInteractiveTerminal()
		sawOutput = tui.PromptWriter()
		return &mainManifest{}, nil
	}

	cmd := newOSInstallCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--device-type", "esp32-c6", "--no-wifi"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("ESP32 --device-type unexpectedly accepted")
	}
	if !sawInteractive || sawOutput != terminal {
		t.Fatalf("during the run: interactive=%v output=%v; want true and the terminal", sawInteractive, sawOutput)
	}
	if isInteractiveTerminal() || *closes != 1 {
		t.Fatalf("after the run: interactive=%v closes=%d; want false and 1", isInteractiveTerminal(), *closes)
	}
}

func TestInstallCommandRejectsBadFlagsBeforeAnyWork(t *testing.T) {
	stubSessionTerminals(t, false, false, nil)
	origFetch := fetchInstallMainManifest
	t.Cleanup(func() { fetchInstallMainManifest = origFetch })
	fetchInstallMainManifest = func(context.Context, int) (*mainManifest, error) {
		t.Fatal("fetched the manifest before rejecting the flags")
		return nil, nil
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--device-type", "raspberry-pi-5", "--no-wifi", "--wifi-password", "pw"}, "--no-wifi cannot be combined"},
		{[]string{"--device-type", "raspberry-pi-5", "--no-wifi", "--wifi", "ssid=Home"}, "--no-wifi cannot be combined"},
		{[]string{"--device-type", "raspberry-pi-5", "--wifi-password", "pw"}, "--wifi-password requires --wifi-ssid"},
		{[]string{"--device-type", "raspberry-pi-5", "--wifi", "password=pw"}, "ssid is required"},
		{[]string{"--device-type", "raspberry-pi-5", "--no-wifi", "--device-name", "Lab_1"}, "--device-name"},
	} {
		cmd := newOSInstallCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(tc.args)
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), tc.want) || !IsUsageError(err) {
			t.Errorf("%v: got %v; want a usage error containing %q", tc.args, err, tc.want)
		}
	}
}

// stubInstallConfirms records the install confirm prompts and answers answer.
func stubInstallConfirms(t *testing.T, answer bool) *[]string {
	t.Helper()
	origPlain, origDanger := installConfirmPrompt, installDangerConfirmPrompt
	t.Cleanup(func() { installConfirmPrompt, installDangerConfirmPrompt = origPlain, origDanger })
	var asked []string
	installConfirmPrompt = func(q string) (bool, error) { asked = append(asked, "plain:"+q); return answer, nil }
	installDangerConfirmPrompt = func(q string) (bool, error) { asked = append(asked, "danger:"+q); return answer, nil }
	return &asked
}

func setInteractive(t *testing.T, interactive bool) {
	t.Helper()
	orig := isInteractiveTerminalFn
	t.Cleanup(func() { isInteractiveTerminalFn = orig })
	isInteractiveTerminalFn = func() bool { return interactive }
}

func TestConfirmInstallStep(t *testing.T) {
	t.Run("force skips the question", func(t *testing.T) {
		setInteractive(t, false)
		asked := stubInstallConfirms(t, false)
		ok, err := confirmInstallStep("flashing the Thor", "Flash?", true, true)
		if !ok || err != nil || len(*asked) != 0 {
			t.Fatalf("got %v, %v, asked %v; want true, nil, nothing asked", ok, err, *asked)
		}
	})
	t.Run("nobody to answer", func(t *testing.T) {
		setInteractive(t, false)
		asked := stubInstallConfirms(t, true)
		ok, err := confirmInstallStep("flashing the Thor", "Flash?", true, false)
		if ok || err == nil || len(*asked) != 0 {
			t.Fatalf("got %v, %v, asked %v; want an error and nothing asked", ok, err, *asked)
		}
		want := "flashing the Thor needs confirmation: run this in a terminal, or pass --force"
		if err.Error() != want || !IsUsageError(err) {
			t.Fatalf("error = %q; want usage error %q", err, want)
		}
	})
	t.Run("danger prompt", func(t *testing.T) {
		setInteractive(t, true)
		asked := stubInstallConfirms(t, true)
		if ok, err := confirmInstallStep("x", "Flash?", true, false); !ok || err != nil {
			t.Fatalf("got %v, %v", ok, err)
		}
		if len(*asked) != 1 || (*asked)[0] != "danger:Flash?" {
			t.Fatalf("asked %v; want the danger prompt", *asked)
		}
	})
	t.Run("plain prompt declined", func(t *testing.T) {
		setInteractive(t, true)
		asked := stubInstallConfirms(t, false)
		if ok, err := confirmInstallStep("x", "Ready?", false, false); ok || err != nil {
			t.Fatalf("got %v, %v; want false, nil", ok, err)
		}
		if len(*asked) != 1 || (*asked)[0] != "plain:Ready?" {
			t.Fatalf("asked %v; want the plain prompt", *asked)
		}
	})
}

func TestCheckInstallConfirmable(t *testing.T) {
	setInteractive(t, false)
	if err := checkInstallConfirmable("flashing the Thor", true); err != nil {
		t.Fatalf("--force: %v", err)
	}
	if err := checkInstallConfirmable("flashing the Thor", false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("non-interactive without --force: %v", err)
	}
	setInteractive(t, true)
	if err := checkInstallConfirmable("flashing the Thor", false); err != nil {
		t.Fatalf("interactive: %v", err)
	}
}

func TestInstallPickersNeedATerminal(t *testing.T) {
	setInteractive(t, false)

	_, err := pickInstallDevice([]tui.PickerItem{
		{Name: "Raspberry Pi 5", Value: "raspberry-pi-5"},
		{Name: "Jetson AGX Thor", Value: thorDeviceType},
		{Name: "ESP32-C6", Value: "wlite_target_esp32c6"},
		{Name: "Linux Desktop", Value: linuxDesktopValue},
	})
	if err == nil || !IsUsageError(err) {
		t.Fatalf("device picker: %v; want a usage error", err)
	}
	for _, want := range []string{"--device-type", "raspberry-pi-5", thorDeviceType, linuxDesktopValue} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("device picker error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "wlite_") {
		t.Errorf("device picker error %q offers a Wendy Lite value no flag accepts", err)
	}

	if _, err := pickExternalDrive(context.Background()); err == nil || !strings.Contains(err.Error(), "--drive") {
		t.Fatalf("drive picker: %v; want an error naming --drive", err)
	}
	if err := requireRecoveryChoice(2, "Thor"); err == nil || !strings.Contains(err.Error(), "2 Thor boards") {
		t.Fatalf("recovery picker: %v", err)
	}

	setInteractive(t, true)
	if err := requireInstallChoice("choosing a drive", "pass --drive"); err != nil {
		t.Fatalf("interactive picker refused: %v", err)
	}
	if err := requireRecoveryChoice(2, "Thor"); err != nil {
		t.Fatalf("interactive recovery picker refused: %v", err)
	}
}

func TestPromptRescan(t *testing.T) {
	var out bytes.Buffer
	t.Cleanup(tui.SetPromptOutput(&out))

	setInteractive(t, false)
	if !promptRescan("Press Enter to rescan: ") {
		t.Fatal("non-interactive rescan prompt did not quit")
	}
	if out.Len() != 0 {
		t.Fatalf("non-interactive rescan drew %q", out.String())
	}

	setInteractive(t, true)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin; _ = r.Close() })
	_, _ = w.WriteString("\n")
	_ = w.Close()
	if promptRescan("Press Enter to rescan: ") {
		t.Fatal("Enter was read as quit")
	}
	if out.String() != "Press Enter to rescan: " {
		t.Fatalf("prompt output = %q; want the question", out.String())
	}
}

func TestConfirmOverwriteInternalDriveNeedsATerminal(t *testing.T) {
	internal := drive{Name: "Samsung SSD", DevicePath: "/dev/sdb"}
	origRead := readLineFn
	t.Cleanup(func() { readLineFn = origRead })
	reads := 0
	typed := ""
	readLineFn = func() string { reads++; return typed }

	setInteractive(t, false)
	if err := confirmOverwriteInternalDrive(drive{IsRemovable: true}, false, false); err != nil {
		t.Fatalf("removable drive: %v", err)
	}
	if err := confirmOverwriteInternalDrive(internal, false, true); err != nil {
		t.Fatalf("--yes-overwrite-internal: %v", err)
	}
	if err := confirmOverwriteInternalDrive(internal, true, false); err == nil {
		t.Fatal("--force alone accepted an internal drive")
	}
	err := confirmOverwriteInternalDrive(internal, false, false)
	if err == nil || !strings.Contains(err.Error(), "--yes-overwrite-internal") || reads != 0 {
		t.Fatalf("non-interactive: %v after %d reads; want an error naming --yes-overwrite-internal and no read", err, reads)
	}

	setInteractive(t, true)
	var out bytes.Buffer
	t.Cleanup(tui.SetPromptOutput(&out))
	typed = "/dev/sdb"
	if err := confirmOverwriteInternalDrive(internal, false, false); err != nil {
		t.Fatalf("typed path: %v", err)
	}
	if !strings.Contains(out.String(), "type the device path exactly") {
		t.Fatalf("prompt output = %q; want the typed-confirm question", out.String())
	}
	typed = "/dev/sda"
	if err := confirmOverwriteInternalDrive(internal, false, false); err == nil {
		t.Fatal("a mistyped path was accepted")
	}
}

func TestConfirmEraseDrive(t *testing.T) {
	d := drive{Name: "SD Card", DevicePath: "/dev/disk4"}

	setInteractive(t, false)
	asked := stubInstallConfirms(t, true)
	err := confirmEraseDrive(d, false)
	if err == nil || !strings.Contains(err.Error(), "erasing SD Card (/dev/disk4) needs confirmation") || len(*asked) != 0 {
		t.Fatalf("non-interactive: %v, asked %v", err, *asked)
	}
	if err := confirmEraseDrive(d, true); err != nil || len(*asked) != 0 {
		t.Fatalf("--force: %v, asked %v", err, *asked)
	}

	setInteractive(t, true)
	t.Cleanup(tui.SetPromptOutput(io.Discard))
	asked = stubInstallConfirms(t, false)
	if err := confirmEraseDrive(d, false); !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("declined erase: %v; want ErrUserCancelled", err)
	}
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "ERASE ALL DATA on SD Card (/dev/disk4)") {
		t.Fatalf("asked %v", *asked)
	}
}

func TestStderrPromptWriterFollowsPromptOutput(t *testing.T) {
	if got := stderrPromptWriter(); got != os.Stderr {
		t.Fatalf("without a prompt output: %v; want os.Stderr", got)
	}
	var out bytes.Buffer
	t.Cleanup(tui.SetPromptOutput(&out))
	if got := stderrPromptWriter(); got != &out {
		t.Fatalf("with a prompt output: %v; want it", got)
	}
}

// The access-denied line is one plain sentence with one step that works on
// every distro: no udev or group vocabulary, which would be wrong on Arch.
func TestUSBAccessDeniedLine(t *testing.T) {
	got := usbAccessDeniedLine("Jetson")
	for _, want := range []string{"Jetson", "over USB", "again with sudo"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %q", want, got)
		}
	}
	for _, jargon := range []string{"udev", "plugdev", "libusb", "\n"} {
		if strings.Contains(got, jargon) {
			t.Errorf("must not contain %q: %q", jargon, got)
		}
	}
}
