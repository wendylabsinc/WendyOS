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
	go2InstallName    = "Unitree Go2 EDU (Orin NX 16 GB)"
	go2RecoveryPort   = "the Go2 expansion dock's recovery USB-C port beside the BAT and RJ45 connectors"
	go2RecoveryEntry  = "Power off the Go2 and disconnect the dock's BAT power. Hold the recessed recovery button near RJ45 with a SIM eject tool while reconnecting BAT. Hold it for about two seconds, then release it."
	go2CarrierCheck   = "Confirm the USB cable is connected to the Go2 EDU expansion dock. USB recovery identifies the Orin NX module only."
	g1PC2InstallName  = "Unitree G1 EDU PC2 (Orin NX 16 GB)"
	g1PC2Port         = "the PC2 flashing USB-C port below the PWR and REC buttons inside the G1's rear compartment"
	g1PC2Entry        = "Turn on the G1 and wait for three steady PC2 lights. Hold PWR and REC together for two seconds. Let go of PWR first and release REC once two lights remain."
	g1PC2CarrierCheck = "Confirm you are connected to the G1 EDU PC2. USB recovery identifies the Orin NX module only. Use a USB 2.0 data cable to keep the flashing connection at the same USB path."
)

type unitreeRecoveryProfile struct {
	name, carrier, port, entry, carrierCheck, docsAnchor string
}

func unitreeRecovery(deviceType string) unitreeRecoveryProfile {
	if deviceType == g1PC2DeviceType {
		return unitreeRecoveryProfile{g1PC2InstallName, "g1-pc2", g1PC2Port, g1PC2Entry, g1PC2CarrierCheck, "unitree-g1-edu-pc2"}
	}
	return unitreeRecoveryProfile{go2InstallName, "go2-edu", go2RecoveryPort, go2RecoveryEntry, go2CarrierCheck, "unitree-go2-edu"}
}

func checkUnitreeRecoveryOptions(deviceType string, rootfsOnly bool, storage string) error {
	if rootfsOnly {
		return usageErrorf("%s installs over USB recovery; omit --rootfs-only", deviceType)
	}
	if storage != "" && storage != "nvme" {
		return usageErrorf("%s recovery supports NVMe only", deviceType)
	}
	return nil
}

func installUnitreeFromArchive(ctx context.Context, path string, opts t234InstallOptions) (retErr error) {
	ctx, finish := startActivationOperation(ctx, "install", "wendy install")
	defer func() { finish(retErr) }()
	if err := opts.Agent.checkArchitecture("arm64"); err != nil {
		return err
	}
	if err := checkInstallConfirmable("flashing the "+opts.DeviceName, opts.Force); err != nil {
		return err
	}
	if err := elevateForT234Flash(opts.DeviceType, false); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "wendy-recovery-flashpack-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	fmt.Printf("Extracting and verifying the local recovery flashpack for %s…\n", opts.DeviceName)
	fp, err := flashpack.OpenRecoveryArchive(path, filepath.Join(dir, "flashpack"), opts.DeviceType, opts.Storage)
	if err != nil {
		return err
	}
	opts.Version, opts.Flashpack = fp.Manifest.WendyOSVersion, fp
	return installOrinRestartable(ctx, opts)
}
