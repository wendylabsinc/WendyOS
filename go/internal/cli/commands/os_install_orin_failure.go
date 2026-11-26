//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/t234"
)

// orinEarlyFailureError is the Jetson stopping before it offered its storage
// for writing: it handed back its status package where the rootfs disk was due.
//
// What the board holds by then follows from the recovery initrd's script
// (meta-tegra's init-flash.sh) and the flashpack's command sequence, which is
// "bootloader, extra-pre-wipe, export-devices <disk>, extra, reboot". The
// bootloader command comes first and rewrites QSPI in the background, and the
// script waits for that to end before it reports any status. The storage is
// written only by this computer, which never received it. So by then the QSPI
// rewrite has run, to its end or to an error, and WendyOS has not been
// written, whatever stopped the script.
type orinEarlyFailureError struct {
	status  string // the Jetson's status word, normally FAILED
	failure t234.DeviceFailure
	logDir  string // where the Jetson's logs were saved; empty when it sent none
}

func (e *orinEarlyFailureError) Error() string {
	if dev := e.failure.MissingStorage; dev != "" {
		name, _ := orinMissingStorage(dev)
		return fmt.Sprintf("the Jetson could not find its %s (%s), so WendyOS was not written", name, dev)
	}
	msg := fmt.Sprintf("device failed before rootfs export: status %q", e.status)
	// One line of the log keeps the error to a line; the hint lists them all.
	if len(e.failure.Evidence) > 0 {
		msg += "; device log: " + e.failure.Evidence[0]
	}
	return msg
}

// orinEarlyFailure saves the logs the Jetson handed back with its early status
// and reads them for the reason it stopped.
func orinEarlyFailure(out io.Writer, logPath string, st *t234.FinalStatus) error {
	return &orinEarlyFailureError{
		status:  st.Status,
		failure: t234.DiagnoseDeviceLogs(st.Logs),
		logDir:  saveOrinDeviceLogs(out, logPath, st),
	}
}

// orinHintWidth is where the sentences of the early-failure box wrap. They
// hold the Jetson's device names and status, so their length varies.
const orinHintWidth = 72

func wrapOrinHint(s string) string { return ansi.Wordwrap(s, orinHintWidth, "") }

// orinMissingStorage names the Jetson block device dev in a person's words,
// and says what to do before starting again when the Jetson cannot find it.
func orinMissingStorage(dev string) (name string, advice []string) {
	switch base := path.Base(dev); {
	case strings.HasPrefix(base, "nvme"):
		return "NVMe SSD", []string{
			"  1. Unplug the Jetson's power.",
			"  2. Seat the NVMe SSD firmly in its M.2 slot.",
			"  3. Put the Jetson back into Force Recovery mode.",
		}
	case strings.HasPrefix(base, "mmcblk"):
		return "eMMC", []string{
			"The eMMC is part of the Jetson module, so it cannot be reseated.",
			wrapOrinHint("Put the Jetson back into Force Recovery mode before starting again. If the eMMC is still not found, it may be faulty. An NVMe SSD can be used instead (--storage nvme)."),
		}
	default:
		return "storage device", []string{
			"  1. Unplug the Jetson's power.",
			"  2. Check that the storage device is connected to the Jetson.",
			"  3. Put the Jetson back into Force Recovery mode.",
		}
	}
}

// printOrinEarlyFailureHint says why the Jetson stopped before WendyOS was
// written, what that left on the board, and what to do before starting again.
// A cause its logs do not name is shown as the last lines of those logs.
// withCommand adds the command that starts again, for a run that cannot ask.
func printOrinEarlyFailureHint(w io.Writer, opts t234InstallOptions, f *orinEarlyFailureError, withCommand bool) {
	firmware := "The Jetson may have started rewriting its boot firmware (QSPI). It may not boot until the install completes."
	if f.failure.BootFirmwareStarted {
		firmware = "The Jetson had already started rewriting its boot firmware (QSPI). It may not boot until the install completes."
	}
	var title string
	var body, below []string
	if dev := f.failure.MissingStorage; dev != "" {
		name, advice := orinMissingStorage(dev)
		title = "The Jetson could not find its " + name
		body = append(body,
			wrapOrinHint(fmt.Sprintf("It waited for the %s (%s), which never appeared. WendyOS was not written.", name, dev)),
			wrapOrinHint(firmware),
			"")
		body = append(body, advice...)
	} else {
		title = "The Jetson stopped before WendyOS was written"
		logs := "It sent no logs."
		next := "Put the Jetson back into Force Recovery mode before starting again."
		if len(f.failure.Evidence) > 0 {
			logs = "The last lines of its log are below."
			next += " If it stops the same way, the device logs show where."
			below = append(below, "Last lines of the device log:")
			for _, line := range f.failure.Evidence {
				below = append(below, "  "+line)
			}
		}
		body = append(body,
			wrapOrinHint(fmt.Sprintf("It reported %q before it offered its storage for writing. WendyOS was not written.", f.status)),
			logs,
			wrapOrinHint(firmware),
			"",
			wrapOrinHint(next))
	}
	fmt.Fprintln(w, "\n"+thorHintBorder.Render(thorHintTitle.Render("⚠  "+title)+"\n\n"+strings.Join(body, "\n")))
	// Log lines and paths go below the box, whole: the box cannot wrap them.
	if f.logDir != "" {
		below = append(below, "Device logs: "+f.logDir)
	}
	if len(below) > 0 {
		fmt.Fprintln(w, "\n"+strings.Join(below, "\n"))
	}
	if withCommand {
		fmt.Fprintln(w, "\nThen start the installation again:")
		fmt.Fprintln(w, orinInstallCommand(opts))
	}
}

// orinCanAskToStartAgain reports whether a stopped install may ask to start
// again: a person is at the terminal, and --force did not promise a run
// without questions.
func orinCanAskToStartAgain(opts t234InstallOptions) bool {
	return !opts.Force && !jsonOutput && isInteractiveTerminal()
}

// installOrinRestartable runs installOrin, and runs it again from the start
// each time the Jetson stops early and the person asks for that. Nothing of a
// stopped run can be resumed: the Jetson reboots out of recovery mode once
// this computer has read its status.
func installOrinRestartable(ctx context.Context, opts t234InstallOptions) error {
	return restartOrinAfterEarlyFailure(opts, func() error { return installOrin(ctx, opts) })
}

func restartOrinAfterEarlyFailure(opts t234InstallOptions, install func() error) error {
	for {
		err := install()
		var early *orinEarlyFailureError
		if !errors.As(err, &early) || !orinCanAskToStartAgain(opts) || !confirmFn("Start the install again?") {
			return err
		}
	}
}
