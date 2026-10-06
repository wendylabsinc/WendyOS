//go:build darwin || linux

package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
)

func listInstallJobRecoveryDevices() ([]rcm.RecoveryDevice, error) { return rcm.ListRecoveryDevices() }

func installJobPrivileges(ctx context.Context, j *onboarding.Job) error {
	if os.Geteuid() == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "sudo", "-n", "true").Run(); err == nil {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return fmt.Errorf("administrator access is required. In a terminal, run sudo %s install jobs resume --job-root %s --job-id %s --target-id %s --confirm-erase%s; this continues the same job after password entry", shellQuoteArg(self), shellQuoteArg(j.StorePath), j.ID, j.Target.Fingerprint, installJobInternalFlag(j))
}

func installJobInternalFlag(j *onboarding.Job) string {
	if j.Plan.Method != "usb-recovery" && !j.Target.Removable {
		return " --confirm-internal"
	}
	return ""
}

func installJobProcess(self string, args []string) *exec.Cmd {
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command(self, args...)
	} else {
		cmd = exec.Command("sudo", append([]string{"-n", thorSudoPreserveEnv, self}, args...)...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if base, err := os.UserCacheDir(); err == nil {
		cmd.Env = pinCacheDirEnv(os.Environ(), base)
	}
	return cmd
}

func installJobDriveIdentity(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.CommandContext(ctx, "diskutil", "info", path)
	} else {
		cmd = exec.CommandContext(ctx, "lsblk", "--nodeps", "--noheadings", "--output", "SERIAL,WWN", path)
	}
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("reading drive identity: %w", err)
	}
	if runtime.GOOS == "darwin" {
		// Retain stable device fields only. Mount state and free space change
		// between preparation and the final pre-write probe.
		var identity []string
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			for _, key := range []string{"Device / Media Name:", "Device Location:", "Protocol:", "Disk / Partition UUID:", "Media UUID:", "Device Tree Path:", "Serial Number:"} {
				if strings.HasPrefix(line, key) {
					identity = append(identity, line)
					break
				}
			}
		}
		if len(identity) == 0 {
			return "", fmt.Errorf("diskutil returned no target identity")
		}
		return strings.Join(identity, "; "), nil
	}
	identity := strings.TrimSpace(string(b))
	if identity == "" {
		return "No hardware serial exposed; identity is limited to drive path, model, capacity and removable status.", nil
	}
	return "Serial/WWN: " + identity, nil
}
