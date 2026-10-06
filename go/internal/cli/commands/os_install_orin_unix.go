//go:build darwin || linux

package commands

// macOS/Linux hooks for the shared T234 Orin install flow: recovery-device
// enumeration and stage-1 RCM boot use gousb (packages rcm/bringup), and raw
// block writes run in the hidden `wendy __t234-write` helper re-exec'd under
// sudo — the unprivileged parent keeps the UI and USB scanning.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/bringup"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashpack"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/t234"
)

// pickOrinRecoveryDevice lists T234 modules in recovery mode and selects one,
// filtered to the install's module family.
func pickOrinRecoveryDevice(opts t234InstallOptions) (rcm.RecoveryDevice, error) {
	return pickUnixRecoveryDevice(orinRecoveryHints(opts), orinRecoveryMatch(opts.DeviceType))
}

// orinStageOne performs the stage-1 RCM boot over gousb with the file chain
// declared by the flashpack manifest.
func orinStageOne(fp *flashpack.Flashpack, dev rcm.RecoveryDevice, out io.Writer) error {
	return orinStageOneVerified(fp, dev, out, "")
}

func orinStageOneVerified(fp *flashpack.Flashpack, dev rcm.RecoveryDevice, out io.Writer, expectedECID string) error {
	order, memBCT, blob, err := t234RCMFiles(fp)
	if err != nil {
		return err
	}
	return bringup.Run(bringup.Options{Dir: fp.Root, MemBCT: memBCT, Blob: blob, DevicePath: dev.PathKey,
		ExpectedProduct: dev.Product, ExpectedECID: expectedECID, SendOrder: order, Out: out})
}

// isWinRawDiskAccessError is Windows-only (errno-matched there); never true
// on macOS/Linux.
func isWinRawDiskAccessError(error) bool { return false }

func runT234Helper(ctx context.Context, req t234.HelperRequest, onProgress func(done, total int64)) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating wendy binary: %w", err)
	}
	// SECURITY: the target block device is identified by USB VID:PID before elevation
	// and no NOPASSWD sudoers rule ships, so this re-exec is not an unprivileged
	// escalation. A path glob cannot distinguish the gadget from the boot disk;
	// hardening this means re-resolving the node by port/serial inside the helper.
	cmd := exec.CommandContext(ctx, "sudo", append([]string{self, "__t234-write"}, req.Args()...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
	cmd.WaitDelay = 5 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = &progressWriter{onProgress: onProgress}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting T234 write helper: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w", msg, err)
		}
		return err
	}
	return nil
}
