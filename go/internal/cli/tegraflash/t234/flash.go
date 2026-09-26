//go:build darwin || linux || windows

package t234

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Stage-2 timeouts. The first flashpkg LUN appears once the RCM-booted
// kernel+initrd are up (~30 s); the rootfs LUN only after the device has run
// the earlier command-sequence steps (bootloader → QSPI programming, pre-wipe),
// which can take many minutes on the device before it exports mmcblk0; the
// final status LUN only after all programming completes.
const (
	// flashpkgSize is the exact size exported by tegra-flash-init and of the
	// builder-created schema-v2 flashpkg.ext4 image.
	flashpkgSize    = 128 << 20
	flashpkgWait    = 5 * time.Minute
	rootfsWait      = 15 * time.Minute
	finalStatusWait = 15 * time.Minute
	disappearWait   = 45 * time.Second
)

// identityReattachWait bounds how long the identity read waits for a detached
// flashpkg LUN to re-attach before giving up. A DiskArbitration eject (macOS's
// "disk not readable" dialog defaults to Eject) tears the LUN down mid-read;
// when the gadget re-attaches at all it does so within a few seconds.
var identityReattachWait = 20 * time.Second

// identityRetryDelay spaces the identity-read retries. A same-node transient
// (the raw provider momentarily unavailable while the LUN stays listed) clears
// within about a second — back-to-back retries would exhaust every attempt
// inside the blip — and a real detach needs a moment to register with the OS
// before the re-scan can see the LUN's true state.
var identityRetryDelay = 500 * time.Millisecond

// ErrDeviceSideFailed reports that the device exported its status package
// early, i.e. its side of the flash aborted before the rootfs write.
var ErrDeviceSideFailed = errors.New("device-side flash failed early")

// Stage2 drives the mass-storage half of the flash (everything after the RCM
// boot). Callbacks keep the privileged pieces and the UI in the caller.
type Stage2 struct {
	FlashPackagePath string
	LayoutPath       string
	ImagesDir        string
	Plan             *Plan
	PortPath         string
	Session          string
	StatusPath       string
	LogsPath         string
	ExpectedIdentity IdentityExpectation
	HandoffStarted   bool
	Out              io.Writer    // verbose log
	Detail           func(string) // live one-line progress; may be nil

	// TempDir holds the identity/verify/status handoff files exchanged with the
	// root __t234-write helper. It must be a private (non-world-writable) dir:
	// Linux fs.protected_regular blocks root from O_CREAT-opening a file this
	// unprivileged process owns in a sticky, world-writable dir like /tmp. Empty
	// falls back to os.TempDir().
	TempDir string

	// RunHelper runs one privileged helper request — re-exec'd as `wendy
	// __t234-write` under sudo on macOS/Linux (serialized at that boundary via
	// HelperRequest.Args), executed in-process on Windows — reporting any
	// "PROGRESS <done> <total>" lines it prints.
	RunHelper func(context.Context, HelperRequest, func(done, total int64)) error
}

const DeviceIdentityProtocol = "wendy-t234-recovery-v2"

type DeviceIdentity struct {
	Protocol   string `json:"protocol"`
	SessionID  string `json:"session_id"`
	ModuleID   string `json:"module_id"`
	ModuleSKU  string `json:"module_sku"`
	CarrierID  string `json:"carrier_id"`
	CarrierSKU string `json:"carrier_sku"`
}

type IdentityExpectation struct {
	ModuleID, ModuleSKU, CarrierID, CarrierSKU string
}

func (s *Stage2) detail(format string, args ...any) {
	if s.Detail != nil {
		s.Detail(fmt.Sprintf(format, args...))
	}
}

// SendFlashPackage waits for the device's "flashpkg" LUN, replaces it with
// the prep-built command package, and releases it so the device starts
// executing the command sequence.
func (s *Stage2) SendFlashPackage(ctx context.Context) error {
	s.detail("waiting for the flash-package disk")
	fmt.Fprintln(s.Out, "Waiting for the device's flashpkg USB disk...")
	// PortHint, not exact match: the gadget can train at a different USB speed
	// than the bootROM's recovery device, which moves it to the connector's
	// other root-hub port (e.g. recovery high-speed at usb 1-1, SuperSpeed
	// gadget at usb 1-2 — seen live on an Orin Nano on macOS).
	disk, err := WaitForUMSDiskAt(ctx, LUNSelector{Vendor: FlashpkgVendor, PortPath: s.PortPath, PortHint: true}, flashpkgWait)
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "  flashpkg disk: %s (%d bytes, session %s)\n", disk.DevPath, disk.SizeBytes, disk.Serial)
	if disk.SizeBytes > 0 && disk.SizeBytes < flashpkgSize {
		return fmt.Errorf("flashpkg disk %s is smaller (%d bytes) than the flash package (%d bytes)", disk.DevPath, disk.SizeBytes, flashpkgSize)
	}

	// Identity first, on the untouched LUN: it is the last non-destructive step,
	// needs no unmount (a raw read is safe under any auto-mounted volume), and
	// may re-resolve the disk if the LUN detached and re-attached (WDY-2621).
	disk, err = s.verifyDeviceIdentity(ctx, disk)
	if err != nil {
		return err
	}
	s.adoptGadget(disk)
	s.unmount(ctx, disk)
	s.detail("sending flash commands + bootloader")
	s.HandoffStarted = true
	if err := s.RunHelper(ctx, HelperRequest{Writer: WriterOptions{Device: disk.RawPath, Blob: s.FlashPackagePath}}, nil); err != nil {
		return fmt.Errorf("writing flash package: %w", err)
	}
	if err := s.verifyFlashPackage(ctx, disk); err != nil {
		return err
	}
	return s.release(ctx, disk)
}

// adoptGadget re-pins stage-2 correlation to the identity-verified gadget LUN:
// every later LUN (rootfs export, final status) appears on the gadget's own
// port with its session id, not on the port the bootROM enumerated at.
func (s *Stage2) adoptGadget(disk UMSDisk) {
	if disk.PortPath != "" && disk.PortPath != s.PortPath {
		fmt.Fprintf(s.Out, "  gadget re-enumerated at usb %s (recovery was at usb %s)\n", disk.PortPath, s.PortPath)
		s.PortPath = disk.PortPath
	}
	s.Session = disk.Serial
}

// verifyDeviceIdentity reads the initrd-created device.json before replacing
// the first flashpkg LUN. RCM boot is non-persistent; this is the last check
// before the flash-package handoff starts QSPI/rootfs destruction.
//
// The LUN can detach between enumeration and the read — a DiskArbitration
// eject tears down the raw node under the helper, which surfaces as ENXIO
// ("device not configured", seen live on an Orin Nano on macOS, WDY-2621) —
// so a failed read re-resolves the LUN by port/session and retries. The
// returned disk is the one actually read (its device node may have changed),
// and every later step must use it.
func (s *Stage2) verifyDeviceIdentity(ctx context.Context, disk UMSDisk) (UMSDisk, error) {
	tmp, err := os.CreateTemp(s.TempDir, "t234-identity-*.ext4")
	if err != nil {
		return UMSDisk{}, err
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)
	s.detail("verifying module and carrier identity")
	const identityReadAttempts = 3
	for attempt := 1; ; attempt++ {
		readErr := s.RunHelper(ctx, HelperRequest{Writer: WriterOptions{Device: disk.RawPath, DumpTo: path, DumpBytes: flashpkgSize}}, nil)
		if readErr == nil {
			break
		}
		if ctx.Err() != nil {
			return UMSDisk{}, ctx.Err()
		}
		if attempt == identityReadAttempts {
			return UMSDisk{}, fmt.Errorf("reading device identity: %w", readErr)
		}
		fmt.Fprintf(s.Out, "  warning: identity read failed (%v); re-scanning for the %s disk\n", readErr, disk.Vendor)
		select {
		case <-ctx.Done():
			return UMSDisk{}, ctx.Err()
		case <-time.After(identityRetryDelay):
		}
		re, waitErr := WaitForUMSDiskAt(ctx, LUNSelector{Vendor: disk.Vendor, PortPath: disk.PortPath, PortHint: true, Session: disk.Serial}, identityReattachWait)
		if waitErr != nil {
			if ctx.Err() != nil {
				return UMSDisk{}, ctx.Err()
			}
			// The re-scan can also refuse fail-closed (ambiguous/uncorrelated
			// LUN), not just time out — its verdict must reach the user.
			return UMSDisk{}, fmt.Errorf("reading device identity: %w; the %s disk detached during the read (on macOS, the \"disk not readable\" dialog's Eject button detaches it — choose Ignore) and re-scanning for it failed: %v",
				readErr, disk.Vendor, waitErr)
		}
		// Same size gate SendFlashPackage applied to the LUN it enumerated.
		if re.SizeBytes > 0 && re.SizeBytes < flashpkgSize {
			return UMSDisk{}, fmt.Errorf("re-attached %s disk %s is smaller (%d bytes) than the flash package (%d bytes)", re.Vendor, re.DevPath, re.SizeBytes, flashpkgSize)
		}
		if re.DevPath != disk.DevPath {
			fmt.Fprintf(s.Out, "  %s disk re-attached as %s (was %s)\n", disk.Vendor, re.DevPath, disk.DevPath)
		}
		disk = re
	}
	img, err := os.Open(path)
	if err != nil {
		return UMSDisk{}, err
	}
	defer img.Close()
	data, err := Ext4ReadFile(img, "flashpkg/device.json")
	if err != nil {
		return UMSDisk{}, fmt.Errorf("recovery initrd did not provide device.json; refusing to flash without hardware identity: %w", err)
	}
	var got DeviceIdentity
	if err := json.Unmarshal(data, &got); err != nil {
		return UMSDisk{}, fmt.Errorf("parsing recovery device.json: %w", err)
	}
	if err := validateDeviceIdentity(got, disk.Serial, s.ExpectedIdentity); err != nil {
		return UMSDisk{}, err
	}
	fmt.Fprintf(s.Out, "  identity verified: module P%s-%s, carrier P%s-%s, session %s\n", got.ModuleID, got.ModuleSKU, got.CarrierID, got.CarrierSKU, got.SessionID)
	return disk, nil
}

func validateDeviceIdentity(got DeviceIdentity, session string, want IdentityExpectation) error {
	if got.Protocol != DeviceIdentityProtocol {
		return fmt.Errorf("recovery device protocol %q is unsupported", got.Protocol)
	}
	if got.SessionID == "" || !strings.EqualFold(got.SessionID, session) {
		return fmt.Errorf("recovery device session %q does not match USB LUN session %q", got.SessionID, session)
	}
	// The initrd reports UNKNOWN when it could not parse its own device tree
	// (initrds built before the WendyOS device-identity parsing fix choke on
	// DTB compatibles with a variant suffix, e.g. the Orin Nano Super devkit's
	// "nvidia,p3768-0000+p3767-0005-super"). That is a broken flashpack, not
	// wrong hardware — say so, and still refuse the flash.
	if got.ModuleID == "UNKNOWN" || got.CarrierID == "UNKNOWN" {
		return fmt.Errorf("the recovery initrd could not identify the board's module/carrier — this flashpack's initrd cannot parse this device's identity; retry with a newer WendyOS recovery flashpack that includes the device-identity fix")
	}
	if got.ModuleID != want.ModuleID || got.ModuleSKU != want.ModuleSKU || got.CarrierID != want.CarrierID || got.CarrierSKU != want.CarrierSKU {
		return fmt.Errorf("wrong Jetson hardware: detected module P%s-%s on carrier P%s-%s; flashpack requires module P%s-%s on carrier P%s-%s",
			got.ModuleID, got.ModuleSKU, got.CarrierID, got.CarrierSKU, want.ModuleID, want.ModuleSKU, want.CarrierID, want.CarrierSKU)
	}
	return nil
}

// verifyFlashPackage reads the flash package back from the device right after
// writing it and confirms the command sequence parses and matches what we sent.
// We overwrite the whole 128 MiB LUN as a raw ext4 image — macOS can't mount
// ext4 to edit files in place the way the reference Linux host script does — so
// if that image doesn't land as a filesystem the device can parse, the initrd
// reads a broken command mailbox and stalls without exporting anything, which
// is indistinguishable from a device-side hang until we check. Reading it back
// turns that ambiguity into a clear host-side verdict.
func (s *Stage2) verifyFlashPackage(ctx context.Context, disk UMSDisk) error {
	local, err := os.Open(s.FlashPackagePath)
	if err != nil {
		return err
	}
	defer local.Close()
	want, err := Ext4ReadFile(local, "flashpkg/conf/command_sequence")
	if err != nil {
		return fmt.Errorf("reading local command_sequence: %w", err)
	}

	tmp, err := os.CreateTemp(s.TempDir, "t234-verify-*.ext4")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	s.detail("verifying flash package on device")
	if err := s.RunHelper(ctx, HelperRequest{Writer: WriterOptions{Device: disk.RawPath, DumpTo: tmpPath, DumpBytes: flashpkgSize}}, nil); err != nil {
		return fmt.Errorf("reading back flash package: %w", err)
	}
	img, err := os.Open(tmpPath)
	if err != nil {
		return err
	}
	defer img.Close()
	got, err := Ext4ReadFile(img, "flashpkg/conf/command_sequence")
	if err != nil {
		return fmt.Errorf("the flash package did not land on the device as a valid filesystem "+
			"(can't read command_sequence back: %w) — the raw USB write is corrupt", err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		return fmt.Errorf("flash package verification failed: device command_sequence is %q, expected %q",
			strings.TrimSpace(string(got)), strings.TrimSpace(string(want)))
	}
	fmt.Fprintln(s.Out, "  verified flash package on device (command sequence intact)")
	return nil
}

// WriteRootfsDevice waits for the exported eMMC LUN and writes the GPT and
// every partition image, then releases it. If the device exports its status
// package instead, it returns ErrDeviceSideFailed (collect logs via
// AwaitFinalStatus).
func (s *Stage2) WriteRootfsDevice(ctx context.Context) error {
	s.detail("waiting for the %s disk", s.Plan.RootfsDevice)
	fmt.Fprintf(s.Out, "Waiting for the device to export %s over USB...\n", s.Plan.RootfsDevice)
	disk, err := waitForUMSDiskConfirmed(ctx, LUNSelector{Vendor: s.Plan.RootfsDevice, PortPath: s.PortPath, Session: s.Session}, rootfsWait)
	if err != nil {
		if errors.Is(err, errGotFlashpkg) {
			return ErrDeviceSideFailed
		}
		return err
	}
	fmt.Fprintf(s.Out, "  %s: %s (%d bytes)\n", s.Plan.RootfsDevice, disk.DevPath, disk.SizeBytes)
	if disk.USBSpeedMbps > 0 {
		fmt.Fprintf(s.Out, "  USB link: %d Mb/s\n", disk.USBSpeedMbps)
	}
	if min := s.Plan.MinDeviceSectors() * sectorSize; disk.SizeBytes > 0 && disk.SizeBytes < min {
		return fmt.Errorf("exported %s (%d bytes) is smaller than the flash layout (%d bytes)", s.Plan.RootfsDevice, disk.SizeBytes, min)
	}

	s.unmount(ctx, disk)
	fmt.Fprintf(s.Out, "Writing GPT + %d partitions...\n", len(s.Plan.Partitions))
	start := time.Now()
	var written int64
	err = s.RunHelper(ctx, HelperRequest{Writer: WriterOptions{Device: disk.RawPath, WritePlan: true, LayoutPath: s.LayoutPath, ImagesDir: s.ImagesDir, RootfsDevice: s.Plan.RootfsDevice}},
		func(done, total int64) {
			written = done
			if total > 0 {
				s.detail("%.1f/%.1f GiB · %.1f MiB/s", float64(done)/(1<<30), float64(total)/(1<<30), float64(done)/(1<<20)/time.Since(start).Seconds())
			}
		})
	if err != nil {
		return fmt.Errorf("writing %s: %w", s.Plan.RootfsDevice, err)
	}
	fmt.Fprintf(s.Out, "  partitions written in %v (%.1f MiB/s)\n", time.Since(start).Round(time.Second), float64(written)/(1<<20)/time.Since(start).Seconds())
	// macOS re-probes the disk when the writer closes it and may auto-mount
	// the freshly written FAT config partition; unmount before releasing.
	s.unmount(ctx, disk)
	return s.release(ctx, disk)
}

// FinalStatus is the device-side outcome of the flash.
type FinalStatus struct {
	Success bool
	Status  string            // raw contents of flashpkg/status
	Logs    map[string][]byte // device-side logs, by filename
}

// AwaitFinalStatus waits for the device to re-export the flash package,
// reads the status and device-side logs out of it, and releases the disk
// (which lets the device reboot into the freshly flashed OS).
func (s *Stage2) AwaitFinalStatus(ctx context.Context) (*FinalStatus, error) {
	s.detail("waiting for the device's final status")
	fmt.Fprintln(s.Out, "Waiting for the device to report its final status (QSPI programming can take several minutes)...")
	disk, err := WaitForUMSDiskAt(ctx, LUNSelector{Vendor: FlashpkgVendor, PortPath: s.PortPath, Session: s.Session}, finalStatusWait)
	if err != nil {
		return nil, err
	}
	s.unmount(ctx, disk)

	tmp, err := os.CreateTemp(s.TempDir, "t234-flashpkg-*.ext4")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	s.detail("reading device status + logs")
	if err := s.RunHelper(ctx, HelperRequest{Writer: WriterOptions{Device: disk.RawPath, DumpTo: tmpPath, DumpBytes: flashpkgSize}}, nil); err != nil {
		return nil, fmt.Errorf("reading back flash package: %w", err)
	}
	if err := s.release(ctx, disk); err != nil {
		// The flash outcome is already on disk; a failed release only delays
		// the device's automatic reboot. Report it but don't fail the flash.
		fmt.Fprintf(s.Out, "  note: releasing the status disk failed (%v); power-cycle the device manually\n", err)
	}

	img, err := os.Open(tmpPath)
	if err != nil {
		return nil, err
	}
	defer img.Close()

	res := &FinalStatus{Logs: map[string][]byte{}}
	statusPath := strings.TrimPrefix(s.StatusPath, "/")
	if statusPath == "" {
		statusPath = "flashpkg/status"
	}
	statusBytes, err := Ext4ReadFile(img, statusPath)
	if err != nil {
		return nil, fmt.Errorf("reading device status from flash package: %w", err)
	}
	res.Status = strings.TrimSpace(string(statusBytes))
	res.Success = res.Status == "SUCCESS"
	logsPath := strings.TrimPrefix(s.LogsPath, "/")
	if logsPath == "" {
		logsPath = "flashpkg/logs"
	}
	if names, err := Ext4ListDir(img, logsPath); err == nil {
		for _, name := range names {
			if data, err := Ext4ReadFile(img, logsPath+"/"+name); err == nil {
				res.Logs[name] = data
			}
		}
	}
	return res, nil
}

// unmount locks/unmounts the LUN's volumes, reporting (not failing on) a
// volume that stayed mounted — the raw write that follows produces the real
// error, and the warning explains it. Routed through the root helper: umount
// (Linux) and diskutil (macOS) need privilege the unprivileged parent lacks.
func (s *Stage2) unmount(ctx context.Context, disk UMSDisk) {
	if err := s.RunHelper(ctx, HelperRequest{Unmount: true, Writer: WriterOptions{Device: disk.DevPath}}, nil); err != nil {
		fmt.Fprintf(s.Out, "  warning: %v\n", err)
	}
}

// release forces the USB-level disconnect the device's initrd waits for,
// then waits for the disk node to actually go away so the next wait can't
// match a stale LUN.
func (s *Stage2) release(ctx context.Context, disk UMSDisk) error {
	fmt.Fprintf(s.Out, "  releasing %s\n", disk.DevPath)
	// Primary: a SCSI eject (START STOP UNIT / power-off), matching the vendor
	// initrd-flash host script. This is the clean per-LUN "host is done" the
	// device's initrd waits for before finalizing the LUN and moving to its
	// next command — e.g. exporting the rootfs device. A USB-level disconnect
	// alone makes the device leave the flashpkg but can be too blunt for it to
	// then bring up the next LUN. Routed through the root helper: eject
	// (Linux/macOS) and udisksctl power-off (denied by polkit for a non-root,
	// seatless SSH session) need privilege the unprivileged parent lacks —
	// running it here left the LUN unreleased and forced the unbind fallback.
	if err := s.RunHelper(ctx, HelperRequest{Eject: true, Writer: WriterOptions{Device: disk.DevPath}}, nil); err != nil {
		fmt.Fprintf(s.Out, "  warning: eject failed (%v); trying a USB disconnect\n", err)
	}
	gone, err := s.waitForDiskGone(ctx, disk)
	if err != nil {
		return err
	}
	if gone {
		return nil
	}

	// Fallback: force a USB-level disconnect (the initrd also proceeds when the
	// UDC leaves "configured"). Needed when the eject didn't take — e.g. no
	// udisks/diskutil or a driver holding the node.
	fmt.Fprintf(s.Out, "  eject didn't release %s; forcing a USB disconnect\n", disk.DevPath)
	if err := s.RunHelper(ctx, HelperRequest{Release: true, ReleaseSerial: disk.Serial, ReleasePort: disk.PortPath}, nil); err != nil {
		return fmt.Errorf("releasing %s: %w", disk.DevPath, err)
	}
	gone, err = s.waitForDiskGone(ctx, disk)
	if err != nil {
		return err
	}
	if gone {
		return nil
	}
	return fmt.Errorf("%s did not disconnect after release", disk.DevPath)
}

// waitForDiskGone polls until the LUN's device node disappears (up to
// disappearWait) so the next wait can't match a stale node. Returns (false,
// nil) on timeout and (false, ctx.Err()) if the context is cancelled.
func (s *Stage2) waitForDiskGone(ctx context.Context, disk UMSDisk) (bool, error) {
	deadline := time.Now().Add(disappearWait)
	for time.Now().Before(deadline) {
		disks, err := scanUMSDisks()
		gone := err == nil
		for _, d := range disks {
			if d.DevPath == disk.DevPath && d.Vendor == disk.Vendor && d.PortPath == disk.PortPath && strings.EqualFold(d.Serial, disk.Serial) {
				gone = false
			}
		}
		if gone {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(umsPollInterval):
		}
	}
	return false, nil
}

// errGotFlashpkg distinguishes "the device exported its status package
// instead of the requested LUN" inside waitForUMSDiskConfirmed.
var errGotFlashpkg = errors.New("got flashpkg instead of the requested LUN")

// waitForUMSDiskConfirmed is WaitForUMSDisk, except a flashpkg sighting is
// only treated as a device-side failure when it persists across scans (a
// just-released flashpkg node can linger for a moment on the host).
func waitForUMSDiskConfirmed(ctx context.Context, selector LUNSelector, timeout time.Duration) (UMSDisk, error) {
	deadline := time.Now().Add(timeout)
	flashpkgStreak := 0
	for {
		disks, err := scanUMSDisks()
		if err == nil {
			var matches []UMSDisk
			sawFlashpkg := false
			for i, d := range disks {
				if d.Vendor == selector.Vendor && d.PortPath == selector.PortPath && strings.EqualFold(d.Serial, selector.Session) {
					matches = append(matches, disks[i])
				} else if d.Vendor == selector.Vendor && d.PortPath == "" {
					return UMSDisk{}, fmt.Errorf("USB storage %q appeared as %s without physical-port correlation", selector.Vendor, d.DevPath)
				}
				if d.Vendor == FlashpkgVendor && d.PortPath == selector.PortPath && strings.EqualFold(d.Serial, selector.Session) {
					sawFlashpkg = true
				}
			}
			if len(matches) > 1 {
				return UMSDisk{}, fmt.Errorf("multiple USB storage LUNs match %q at port %q/session %q", selector.Vendor, selector.PortPath, selector.Session)
			}
			if len(matches) == 1 {
				return matches[0], nil
			}
			if sawFlashpkg {
				flashpkgStreak++
				if flashpkgStreak >= 5 {
					return UMSDisk{}, errGotFlashpkg
				}
			} else {
				flashpkgStreak = 0
			}
		}
		if time.Now().After(deadline) {
			return UMSDisk{}, fmt.Errorf("timed out waiting for USB storage %q from the selected device\n%s", selector.Vendor, observedUMSHint())
		}
		select {
		case <-ctx.Done():
			return UMSDisk{}, ctx.Err()
		case <-time.After(umsPollInterval):
		}
	}
}
