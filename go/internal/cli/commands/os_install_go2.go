//go:build darwin || linux || windows

package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashpack"
)

const (
	go2InstallName   = "Unitree Go2 EDU (Orin NX 16 GB)"
	go2RecoveryPort  = "the Go2 expansion dock's recovery USB-C port beside the BAT and RJ45 connectors"
	go2RecoveryEntry = "Power off the Go2 and disconnect the dock's BAT power. Hold the recessed recovery button near RJ45 with a SIM eject tool while reconnecting BAT. Hold it for about two seconds, then release it."
	go2CarrierCheck  = "Confirm the USB cable is connected to the Go2 EDU expansion dock. USB recovery identifies the Orin NX module only."
)

func checkGo2RecoveryOptions(rootfsOnly bool, storage string) error {
	if rootfsOnly {
		return usageErrorf("unitree-go2 installs over USB recovery; omit --rootfs-only")
	}
	if storage != "" && storage != "nvme" {
		return usageErrorf("unitree-go2 recovery supports NVMe only")
	}
	return nil
}

func installGo2FromArchive(ctx context.Context, path string, opts t234InstallOptions) (retErr error) {
	ctx, finish := startActivationOperation(ctx, "install", "wendy install")
	defer func() { finish(retErr) }()
	if err := opts.Agent.checkArchitecture("arm64"); err != nil {
		return err
	}
	if err := checkInstallConfirmable("flashing the "+opts.DeviceName, opts.Force); err != nil {
		return err
	}
	if err := elevateForT234Flash(go2DeviceType, false); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "wendy-go2-flashpack-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	fmt.Println("Extracting and verifying the local Go2 recovery flashpack…")
	fp, err := flashpack.OpenRecoveryArchive(path, filepath.Join(dir, "flashpack"), go2DeviceType, opts.Storage)
	if err != nil {
		return err
	}
	opts.Version, opts.Flashpack = fp.Manifest.WendyOSVersion, fp
	return installOrinRestartable(ctx, opts)
}
