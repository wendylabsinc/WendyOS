//go:build darwin || linux || windows

package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/t234"
)

// orinBootloaderLogHead is how the Jetson's bootloader.log starts once it has
// begun rewriting QSPI.
const orinBootloaderLogHead = "Erasing /dev/mtd0\nWriting br_bct_BR.bct (size=8192) to BCT (offset=0)\nCopied 8192 bytes from br_bct_BR.bct to address 0x00000000 in flash\n"

// orinNoExtraScriptLog is the log every WendyOS recovery image writes for the
// extra step it does not have.
const orinNoExtraScriptLog = "No init_extra_pre_wipe was found, ignoring\n"

const (
	orinFirmwareStarted = "The Jetson had already started rewriting its boot firmware (QSPI). It may not boot until the install completes."
	orinFirmwareUnknown = "The Jetson may have started rewriting its boot firmware (QSPI). It may not boot until the install completes."
)

// orinEarlyFailureFrom runs the early-failure step on a status package holding
// logs. It returns the step's error and output, and where the logs must be.
func orinEarlyFailureFrom(t *testing.T, status string, logs map[string]string) (early *orinEarlyFailureError, stepOutput, logDir string) {
	t.Helper()
	st := &t234.FinalStatus{Status: status, Logs: map[string][]byte{}}
	for name, text := range logs {
		st.Logs[name] = []byte(text)
	}
	logPath := filepath.Join(t.TempDir(), "orin-recovery-20261123-120000.log")
	var out strings.Builder
	err := orinEarlyFailure(&out, logPath, st)
	if !errors.As(err, &early) {
		t.Fatalf("early failure error = %T: %v", err, err)
	}
	return early, out.String(), strings.TrimSuffix(logPath, ".log") + "-device-logs"
}

// orinEarlyFailureHint renders the hint for early and splits it into the box,
// as running text so a sentence reads the same wherever its line wrapped, and
// the plain lines printed below the box.
func orinEarlyFailureHint(t *testing.T, opts t234InstallOptions, early *orinEarlyFailureError, withCommand bool) (box, below string) {
	t.Helper()
	var hint strings.Builder
	printOrinEarlyFailureHint(&hint, opts, early, withCommand)
	plain := ansi.Strip(hint.String())
	end := strings.LastIndex(plain, "╯")
	if end < 0 {
		t.Fatalf("hint has no box:\n%s", plain)
	}
	if withCommand {
		assertCommandBelowBox(t, hint.String(), "wendy os install --device-type "+opts.DeviceType+" --storage "+opts.Storage)
	} else if strings.Contains(plain, "wendy os install") {
		t.Fatalf("command printed beside the question:\n%s", plain)
	}
	words := strings.Fields(strings.Map(func(r rune) rune {
		if strings.ContainsRune("│╭╮╰╯─", r) {
			return ' '
		}
		return r
	}, plain[:end]))
	return strings.Join(words, " "), plain[end+len("╯"):]
}

// assertInOrder fails unless text holds every want, each after the one before.
func assertInOrder(t *testing.T, text string, want ...string) {
	t.Helper()
	rest := text
	for _, w := range want {
		i := strings.Index(rest, w)
		if i < 0 {
			t.Fatalf("missing, or out of order: %q\n%s", w, text)
		}
		rest = rest[i+len(w):]
	}
}

// boxLines returns the text lines inside a rendered hint box.
func boxLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(ansi.Strip(output), "\n") {
		if strings.HasPrefix(line, "│") {
			if text := strings.TrimSpace(strings.Trim(line, "│")); text != "" {
				lines = append(lines, text)
			}
		}
	}
	return lines
}

// Each row is the logs a Jetson hands back when it cannot find the storage to
// install to, and what the person is then told.
func TestOrinEarlyFailureNamesTheMissingStorageAndWhatToDo(t *testing.T) {
	for _, tc := range []struct {
		name           string
		board, storage string
		logs           map[string]string
		wantErr        string
		want           []string // in the box, in this order
	}{
		{
			name: "NVMe SSD not seated", board: orinNanoDeviceType, storage: "nvme",
			logs: map[string]string{
				"bootloader.log":            orinBootloaderLogHead,
				"custom-extra-pre-wipe.log": orinNoExtraScriptLog,
				"export-nvme0n1.log":        "Waiting for /dev/nvme0n1.................[FAIL]\n",
			},
			wantErr: "the Jetson could not find its NVMe SSD (/dev/nvme0n1), so WendyOS was not written",
			want: []string{
				"⚠ The Jetson could not find its NVMe SSD",
				"It waited for the NVMe SSD (/dev/nvme0n1), which never appeared. WendyOS was not written.",
				orinFirmwareStarted,
				"1. Unplug the Jetson's power.",
				"2. Seat the NVMe SSD firmly in its M.2 slot.",
				"3. Put the Jetson back into Force Recovery mode.",
			},
		},
		{
			name: "onboard eMMC not found", board: orinDeviceType, storage: "emmc",
			logs: map[string]string{
				"bootloader.log":     orinBootloaderLogHead,
				"export-mmcblk0.log": "Waiting for /dev/mmcblk0.................[FAIL]\n",
			},
			wantErr: "the Jetson could not find its eMMC (/dev/mmcblk0), so WendyOS was not written",
			want: []string{
				"⚠ The Jetson could not find its eMMC",
				"It waited for the eMMC (/dev/mmcblk0), which never appeared. WendyOS was not written.",
				orinFirmwareStarted,
				"The eMMC is part of the Jetson module, so it cannot be reseated.",
				"Put the Jetson back into Force Recovery mode before starting again.",
				"If the eMMC is still not found, it may be faulty.",
				"An NVMe SSD can be used instead (--storage nvme).",
			},
		},
		{
			name: "other storage, as the bug report quotes the line", board: orinDeviceType, storage: "nvme",
			logs: map[string]string{
				"bootloader.log": orinBootloaderLogHead,
				"export-sda.log": "Waiting for /dev/sda ... FAIL\n",
			},
			wantErr: "the Jetson could not find its storage device (/dev/sda), so WendyOS was not written",
			want: []string{
				"⚠ The Jetson could not find its storage device",
				"It waited for the storage device (/dev/sda), which never appeared. WendyOS was not written.",
				orinFirmwareStarted,
				"1. Unplug the Jetson's power.",
				"2. Check that the storage device is connected to the Jetson.",
				"3. Put the Jetson back into Force Recovery mode.",
			},
		},
		{
			name: "no sign the boot firmware was touched", board: orinNanoDeviceType, storage: "nvme",
			logs: map[string]string{
				"export-nvme0n1.log": "Waiting for /dev/nvme0n1.................[FAIL]\n",
			},
			wantErr: "the Jetson could not find its NVMe SSD (/dev/nvme0n1), so WendyOS was not written",
			want: []string{
				"⚠ The Jetson could not find its NVMe SSD",
				orinFirmwareUnknown,
				"2. Seat the NVMe SSD firmly in its M.2 slot.",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			early, stepOutput, logDir := orinEarlyFailureFrom(t, "FAILED", tc.logs)
			if early.Error() != tc.wantErr {
				t.Fatalf("error = %q\nwant    %q", early.Error(), tc.wantErr)
			}
			opts := t234InstallOptions{DeviceType: tc.board, Storage: tc.storage}
			box, below := orinEarlyFailureHint(t, opts, early, true)
			assertInOrder(t, box, tc.want...)
			for _, wrong := range []string{"Recovery was interrupted", "partially written", "retry"} {
				if strings.Contains(box, wrong) {
					t.Fatalf("early failure still says %q:\n%s", wrong, box)
				}
			}
			if want := "\n\nDevice logs: " + logDir + "\n\nThen start the installation again:\nwendy os install --device-type " + tc.board + " --storage " + tc.storage + "\n"; below != want {
				t.Fatalf("below the box = %q\nwant            %q", below, want)
			}

			// The saved-log line and the files it names stay as they were.
			names := make([]string, 0, len(tc.logs))
			for name, text := range tc.logs {
				names = append(names, name)
				if got, err := os.ReadFile(filepath.Join(logDir, name)); err != nil || string(got) != text {
					t.Fatalf("saved %s = %q, %v", name, got, err)
				}
			}
			slices.Sort(names)
			if want := "Device-side logs (" + strings.Join(names, ", ") + "): " + logDir + "\n"; stepOutput != want {
				t.Fatalf("step output = %q\nwant          %q", stepOutput, want)
			}

			// A run that asks to start again prints no command to type.
			if _, below := orinEarlyFailureHint(t, opts, early, false); below != "\n\nDevice logs: "+logDir+"\n" {
				t.Fatalf("below the box, with a question to follow = %q", below)
			}
		})
	}
}

func TestOrinEarlyFailureUnrecognisedShowsTheLastLogLines(t *testing.T) {
	opts := t234InstallOptions{DeviceType: orinNanoDeviceType, Storage: "nvme"}
	command := "\nThen start the installation again:\nwendy os install --device-type " + orinNanoDeviceType + " --storage nvme\n"
	t.Run("failing log lines", func(t *testing.T) {
		early, _, logDir := orinEarlyFailureFrom(t, "FAILED", map[string]string{
			"bootloader.log":            orinBootloaderLogHead + "ERR: could not retrieve file size of mb1.bin\n",
			"commandloop.log":           "Unrecognized command: frobnicate now\n",
			"custom-extra-pre-wipe.log": orinNoExtraScriptLog,
		})
		if want := `device failed before rootfs export: status "FAILED"; device log: commandloop.log: Unrecognized command: frobnicate now`; early.Error() != want {
			t.Fatalf("error = %q\nwant    %q", early.Error(), want)
		}
		box, below := orinEarlyFailureHint(t, opts, early, true)
		assertInOrder(t, box,
			"⚠ The Jetson stopped before WendyOS was written",
			`It reported "FAILED" before it offered its storage for writing. WendyOS was not written.`,
			"The last lines of its log are below.",
			orinFirmwareStarted,
			"Put the Jetson back into Force Recovery mode before starting again.",
			"If it stops the same way, the device logs show where.",
		)
		for _, wrong := range []string{"Recovery was interrupted", "partially written", "could not find"} {
			if strings.Contains(box, wrong) {
				t.Fatalf("unrecognised early failure says %q:\n%s", wrong, box)
			}
		}
		if want := "\n\nLast lines of the device log:\n" +
			"  commandloop.log: Unrecognized command: frobnicate now\n" +
			"  bootloader.log: ERR: could not retrieve file size of mb1.bin\n" +
			"Device logs: " + logDir + "\n" + command; below != want {
			t.Fatalf("below the box = %q\nwant            %q", below, want)
		}
	})
	t.Run("no logs at all", func(t *testing.T) {
		early, stepOutput, logDir := orinEarlyFailureFrom(t, "PENDING: expecting command sequence from host", nil)
		if want := `device failed before rootfs export: status "PENDING: expecting command sequence from host"`; early.Error() != want {
			t.Fatalf("error = %q\nwant    %q", early.Error(), want)
		}
		box, below := orinEarlyFailureHint(t, opts, early, true)
		assertInOrder(t, box,
			"⚠ The Jetson stopped before WendyOS was written",
			`It reported "PENDING: expecting command sequence from host" before it offered its storage for writing. WendyOS was not written.`,
			"It sent no logs.",
			orinFirmwareUnknown,
			"Put the Jetson back into Force Recovery mode before starting again.",
		)
		if below != "\n"+command || stepOutput != "" {
			t.Fatalf("logs reported where the Jetson sent none: step output %q, below the box %q", stepOutput, below)
		}
		if _, err := os.Stat(logDir); !os.IsNotExist(err) {
			t.Fatalf("log directory created for no logs: %v", err)
		}
	})
}

// A failure after the Jetson offered its storage keeps the warning and the
// error it always had, and is never offered a restart.
func TestOrinLateFailureMessageUnchanged(t *testing.T) {
	var hint strings.Builder
	printOrinBadStateHint(&hint, t234InstallOptions{DeviceType: orinNanoDeviceType, Storage: "nvme"})
	if got, want := boxLines(hint.String()), []string{
		"⚠  Recovery was interrupted — the Jetson may not boot",
		"QSPI or the rootfs may be partially written. Re-enter Force Recovery mode.",
		"Then retry with the command below.",
	}; !slices.Equal(got, want) {
		t.Fatalf("late-failure box = %q\nwant               %q", got, want)
	}
	if want := "\nRetry the installation:\nwendy os install --device-type " + orinNanoDeviceType + " --storage nvme\n"; !strings.HasSuffix(hint.String(), want) {
		t.Fatalf("late-failure retry lines changed:\n%s", hint.String())
	}

	stubOrinStartAgain(t, true, false, func(string) bool { t.Fatal("late failure offered a restart"); return false })
	late := fmt.Errorf("device reported final flash status %q", "FAILED")
	attempts := 0
	err := restartOrinAfterEarlyFailure(t234InstallOptions{}, func() error { attempts++; return late })
	if err != late || attempts != 1 || err.Error() != `device reported final flash status "FAILED"` {
		t.Fatalf("late failure: err = %v after %d attempts", err, attempts)
	}
}

// stubOrinStartAgain sets what the restart offer sees: whether a person is at
// a terminal, whether output is JSON, and the answer to the question.
func stubOrinStartAgain(t *testing.T, terminal, jsonMode bool, confirm func(string) bool) {
	t.Helper()
	previousTerminal, previousJSON, previousConfirm := isInteractiveTerminalFn, jsonOutput, confirmFn
	t.Cleanup(func() {
		isInteractiveTerminalFn, jsonOutput, confirmFn = previousTerminal, previousJSON, previousConfirm
	})
	isInteractiveTerminalFn = func() bool { return terminal }
	jsonOutput, confirmFn = jsonMode, confirm
}

func TestOrinEarlyFailureOffersToStartAgain(t *testing.T) {
	early := &orinEarlyFailureError{status: "FAILED"}
	for _, tc := range []struct {
		name                        string
		force, jsonMode, noTerminal bool
		answer                      bool
		wantAttempts, wantQuestions int
		wantErr                     error
	}{
		{name: "yes starts again", answer: true, wantAttempts: 2, wantQuestions: 1},
		{name: "no keeps the failure", answer: false, wantAttempts: 1, wantQuestions: 1, wantErr: early},
		{name: "--force is not asked", force: true, wantAttempts: 1, wantErr: early},
		{name: "--json is not asked", jsonMode: true, wantAttempts: 1, wantErr: early},
		{name: "no terminal is not asked", noTerminal: true, wantAttempts: 1, wantErr: early},
	} {
		t.Run(tc.name, func(t *testing.T) {
			questions := 0
			stubOrinStartAgain(t, !tc.noTerminal, tc.jsonMode, func(question string) bool {
				questions++
				if question != "Start the install again?" {
					t.Fatalf("question = %q", question)
				}
				return tc.answer
			})
			opts := t234InstallOptions{Force: tc.force}
			// The hint prints the command exactly when no question will follow.
			if got, want := !orinCanAskToStartAgain(opts), tc.wantQuestions == 0; got != want {
				t.Fatalf("hint prints the command = %v, want %v", got, want)
			}
			attempts := 0
			err := restartOrinAfterEarlyFailure(opts, func() error {
				attempts++
				if attempts == 1 {
					return fmt.Errorf("step failed: %w", early)
				}
				return nil
			})
			if attempts != tc.wantAttempts || questions != tc.wantQuestions || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
				t.Fatalf("attempts = %d, questions = %d, err = %v; want %d, %d, %v", attempts, questions, err, tc.wantAttempts, tc.wantQuestions, tc.wantErr)
			}
		})
	}
}
