//go:build windows

package commands

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/winusb"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
)

func listInstallJobRecoveryDevices() ([]rcm.RecoveryDevice, error) {
	devices, err := winusb.ListDevices()
	if err != nil {
		return nil, err
	}
	var out []rcm.RecoveryDevice
	for _, d := range devices {
		if d.IsThor() || d.IsT234() {
			out = append(out, d.RecoveryDevice())
		}
	}
	return out, nil
}

func installJobPrivileges(_ context.Context, j *onboarding.Job) error {
	ok, err := isElevated()
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return fmt.Errorf("open an Administrator terminal and resume the same job with wendy install jobs resume --job-root %q --job-id %s --target-id %s --confirm-erase (also --confirm-internal for non-removable host media); prepare the board's Wendy WinUSB driver before recovery", j.StorePath, j.ID, j.Target.Fingerprint)
}

func installJobProcess(self string, args []string) *exec.Cmd {
	cmd := exec.Command(self, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000200 | 0x00000008}
	return cmd
}

func installJobDriveIdentity(ctx context.Context, path string) (string, error) {
	prefix := `\\.\PhysicalDrive`
	if !strings.HasPrefix(path, prefix) {
		return "", fmt.Errorf("invalid physical drive path")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(path, prefix))
	if err != nil || n < 0 {
		return "", fmt.Errorf("invalid physical drive number")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	script := fmt.Sprintf("Get-Disk -Number %d | Select-Object SerialNumber,UniqueId,Path | ConvertTo-Json -Compress", n)
	b, err := exec.CommandContext(ctx, env.PowershellExe(), "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", fmt.Errorf("Get-Disk returned no drive identity")
	}
	return strings.TrimSpace(string(b)), nil
}
