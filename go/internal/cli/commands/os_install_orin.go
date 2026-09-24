//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashpack"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/t234"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

const (
	orinStepDownload = iota
	orinStepProvision
	orinStepRCMBoot
	orinStepCommands
	orinStepWrite
	orinStepStatus
)

// installOrin performs a complete T234 recovery from a pre-signed schema-v2
// flashpack. No NVIDIA host binary or container is used on any platform.
// Per-OS seams: pickOrinRecoveryDevice, orinStageOne, and runT234Helper
// (os_install_orin_unix.go / os_install_orin_windows.go).
func installOrin(ctx context.Context, opts t234InstallOptions) error {
	cacheDir, err := osCacheDir()
	if err != nil {
		return fmt.Errorf("resolving cache dir: %w", err)
	}
	if opts.Artifact == nil {
		return fmt.Errorf("missing recovery artifact metadata")
	}
	ref := flashpack.RecoveryRef{Device: opts.DeviceType, Storage: opts.Storage, Version: opts.Version}

	// Announce a stale cache before the flash UI takes over, mirroring the Thor
	// path. resolveT234Flashpack does the same detection later, but reports it via
	// a step-status line that the download progress immediately overwrites — so the
	// reason for the re-download would otherwise be invisible.
	if extracted := flashpack.RecoveryExtractedCachePath(cacheDir, ref); flashpack.StampStale(extracted, opts.Artifact.Checksum) {
		if _, err := os.Stat(filepath.Join(extracted, "manifest.json")); err == nil {
			fmt.Println(tui.Dim(fmt.Sprintf("Cached WendyOS %s is outdated; downloading the current build.", opts.Version)))
		}
	}

	// Identity/verify/status files pass between this process and the root
	// __t234-write helper. Keep them in a private 0700 dir, not shared /tmp:
	// Linux fs.protected_regular denies root an O_CREAT open of a file this
	// unprivileged process owns in a sticky, world-writable dir.
	handoffDir, err := os.MkdirTemp("", "t234-flash-")
	if err != nil {
		return fmt.Errorf("creating flash temp dir: %w", err)
	}
	defer os.RemoveAll(handoffDir)

	creds, err := resolveWiFiCredentialsList(opts.WiFi)
	if err != nil {
		return err
	}
	name, err := resolveDeviceName(opts.RequestedName)
	if err != nil {
		return err
	}
	provJSON, err := resolveProvisioningJSON(ctx, opts.PreEnroll, name)
	if err != nil {
		return err
	}

	if err := confirmOrinReady(opts); err != nil {
		return err
	}
	dev, err := pickOrinRecoveryDevice(opts)
	if err != nil {
		return err
	}
	fmt.Printf("\n%s %s\n", tui.Dim("Target:"), tui.Device(dev.Describe()))

	if !opts.Force {
		fmt.Println()
		fmt.Println(tui.WarningMessage(fmt.Sprintf("This erases QSPI and all data on the selected %s, including /data. This cannot be undone.", strings.ToUpper(opts.Storage))))
		ok, err := tui.ConfirmNoDefaultDanger(fmt.Sprintf("Flash %s?", dev.Describe()))
		if errors.Is(err, tui.ErrCancelled) || (err == nil && !ok) {
			return ErrUserCancelled
		}
		if err != nil {
			return err
		}
	}

	if err := preAuthElevation(); err != nil {
		return err
	}
	elevationCtx, cancelElevation := context.WithCancel(ctx)
	defer cancelElevation()
	keepElevationAlive(elevationCtx)
	// Otherwise macOS asks about each flashing disk it can't read, and choosing
	// Eject there releases the disk before the flash is done with it.
	if releaseClaims, err := t234.SuppressDiskPrompts(); err != nil {
		fmt.Println(tui.WarningMessage(fmt.Sprintf("Could not stop macOS disk prompts (%v). If macOS says a disk is not readable, choose Ignore.", err)))
	} else {
		defer releaseClaims()
	}
	flashCtx, cancelFlash := context.WithCancel(ctx)
	defer cancelFlash()

	var logW io.Writer = io.Discard
	var logPath string
	if dir, derr := config.LogDir(); derr == nil {
		logPath = filepath.Join(dir, "orin-recovery-"+time.Now().Format("20060102-150405")+".log")
		if lf, lerr := os.Create(logPath); lerr == nil {
			defer lf.Close()
			fmt.Fprintf(lf, "wendy os install — %s %s recovery — WendyOS %s\n\n", opts.DeviceType, opts.Storage, opts.Version)
			logW = lf
			defer func() { fmt.Println(tui.Dim("Full flash log: " + logPath)) }()
		} else {
			logPath = ""
		}
	}

	var (
		fp          *flashpack.Flashpack
		workspace   string
		layoutPath  string
		flashPlan   *t234.Plan
		massStorage *t234.Stage2
		finalStatus *t234.FinalStatus
	)
	defer func() {
		if workspace != "" {
			_ = os.RemoveAll(workspace)
		}
	}()

	boundaryWarning := "Recovery data has been handed to the Jetson — aborting now can leave QSPI or the rootfs partially written. Press ctrl+c again to abort anyway."
	steps := []flashStep{
		{id: orinStepDownload, label: "Download recovery flashpack", run: func(out io.Writer, detail func(string)) (bool, error) {
			var cached bool
			fp, cached, err = resolveT234Flashpack(cacheDir, ref, opts.Artifact, detail)
			return cached, err
		}},
		{id: orinStepProvision, label: "Prepare per-run config", run: func(out io.Writer, detail func(string)) (bool, error) {
			if err := t234.CheckHostTools(); err != nil {
				return false, err
			}
			workspace, layoutPath, err = prepareT234Workspace(fp)
			if err != nil {
				return false, err
			}
			configRel, err := filepath.Rel(fp.FlashImagesDir(), fp.ConfigImage())
			if err != nil {
				return false, err
			}
			if err := injectRecoveryConfig(filepath.Join(workspace, configRel), creds, name, provJSON, out, detail); err != nil {
				return false, err
			}
			flashPlan, err = t234.LoadXMLPlan(layoutPath, workspace, fp.Manifest.RootfsDevice)
			if err != nil {
				return false, err
			}
			target := fp.Manifest.Target
			massStorage = &t234.Stage2{
				FlashPackagePath: fp.FlashPackageImage(), LayoutPath: layoutPath, ImagesDir: workspace,
				Plan: flashPlan, PortPath: dev.PathKey,
				StatusPath: fp.Manifest.Layout.FlashPackageStatus, LogsPath: fp.Manifest.Layout.FlashPackageLogs,
				ExpectedIdentity: t234.IdentityExpectation{ModuleID: target.ModuleID, ModuleSKU: target.ModuleSKU, CarrierID: target.CarrierID, CarrierSKU: target.CarrierSKU},
				Out:              out, Detail: detail, RunHelper: runT234Helper, TempDir: handoffDir,
			}
			return false, nil
		}},
		{id: orinStepRCMBoot, label: "Stage 1  RCM boot", run: func(out io.Writer, _ func(string)) (bool, error) {
			return false, orinStageOne(fp, dev, out)
		}},
		{id: orinStepCommands, label: "Stage 2  verify target + hand off recovery", abortWarning: boundaryWarning, run: func(out io.Writer, detail func(string)) (bool, error) {
			massStorage.Out, massStorage.Detail = out, detail
			return false, massStorage.SendFlashPackage(flashCtx)
		}},
		{id: orinStepWrite, label: "Stage 2  write " + strings.ToUpper(opts.Storage) + " partitions", abortWarning: boundaryWarning, run: func(out io.Writer, detail func(string)) (bool, error) {
			massStorage.Out, massStorage.Detail = out, detail
			err := massStorage.WriteRootfsDevice(flashCtx)
			if errors.Is(err, t234.ErrDeviceSideFailed) {
				finalStatus, _ = massStorage.AwaitFinalStatus(flashCtx)
				if finalStatus != nil {
					saveOrinDeviceLogs(out, logPath, finalStatus)
					return false, fmt.Errorf("device failed before rootfs export: status %q", finalStatus.Status)
				}
			}
			return false, err
		}},
		{id: orinStepStatus, label: "Stage 2  collect final device status", abortWarning: boundaryWarning, run: func(out io.Writer, detail func(string)) (bool, error) {
			massStorage.Out, massStorage.Detail = out, detail
			finalStatus, err = massStorage.AwaitFinalStatus(flashCtx)
			if finalStatus != nil {
				saveOrinDeviceLogs(out, logPath, finalStatus)
			}
			if err != nil {
				return false, err
			}
			if !finalStatus.Success {
				return false, fmt.Errorf("device reported final flash status %q", finalStatus.Status)
			}
			return false, nil
		}},
	}

	failedID, err := runFlashSteps(fmt.Sprintf("Recovering %s with WendyOS %s", opts.DeviceName, opts.Version), steps, cancelFlash, logW)
	if err != nil {
		// The console can vanish with the process (UAC-relaunched window), so
		// the flash log must record why the flash stopped, not just where.
		fmt.Fprintf(logW, "\nFAILED (step %d): %v\n", failedID, err)
		switch {
		case errors.Is(err, tui.ErrCancelled):
			if failedID >= orinStepCommands && massStorage != nil && massStorage.HandoffStarted {
				printOrinBadStateHint(os.Stdout, opts)
			}
			return ErrUserCancelled
		case failedID >= orinStepCommands && isMacRawDiskPermissionError(err):
			// Scoped like the Windows raw-disk case below: only the stage-2
			// steps touch the Jetson's disk, so a permission error earlier
			// (host-file I/O during download/provision) must not print
			// recovery-disk remediation.
			printOrinFullDiskAccessHint(os.Stdout)
		case failedID >= orinStepCommands && isWinRawDiskAccessError(err):
			// Scoped to the stage-2 steps that actually touch the Jetson's
			// disk: a sharing violation on a host file during download or
			// provisioning must not print recovery-disk remediation.
			printOrinWinDiskAccessHint(os.Stdout)
		case errors.Is(err, rcm.ErrUSBAccess):
			fmt.Println("\n" + usbAccessHintBox())
		case failedID >= orinStepCommands && massStorage != nil && massStorage.HandoffStarted:
			printOrinBadStateHint(os.Stdout, opts)
		case failedID == orinStepRCMBoot:
			fmt.Println(tui.WarningMessage("RCM boot failed before persistent writes; re-enter recovery mode and retry."))
		}
		return err
	}
	fmt.Println(tui.SuccessMessage(fmt.Sprintf("Recovered %s %s with WendyOS %s; the Jetson will reboot after the final LUN is released.", opts.DeviceName, strings.ToUpper(opts.Storage), opts.Version)))
	return nil
}

// progressWriter parses the T234 helper's "PROGRESS <done> <total>" stdout
// lines and forwards them to onProgress; anything else is ignored. One decoder
// for both the sudo re-exec (macOS/Linux) and in-process (Windows) helper
// paths, so the wire format cannot drift per platform.
type progressWriter struct {
	onProgress func(done, total int64)
	buf        []byte
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		var done, total int64
		if n, _ := fmt.Sscanf(line, "PROGRESS %d %d", &done, &total); n == 2 && w.onProgress != nil {
			w.onProgress(done, total)
		}
	}
}

func resolveT234Flashpack(cacheDir string, ref flashpack.RecoveryRef, info *recoveryFlashpackInfo, detail func(string)) (*flashpack.Flashpack, bool, error) {
	if info.Checksum == "" {
		return nil, false, fmt.Errorf("manifest entry has no recovery flashpack checksum")
	}
	extracted := flashpack.RecoveryExtractedCachePath(cacheDir, ref)
	tarball := flashpack.RecoveryTarballCachePath(cacheDir, ref)

	// A cached tree whose recorded source checksum still matches the manifest is
	// current — use it. If the manifest republished this version (e.g. a --pr
	// re-push, whose tag is stable), the stamp differs: drop the stale tree and
	// re-download. A tree with no stamp predates stamping and is trusted as-is.
	if _, err := os.Stat(filepath.Join(extracted, "manifest.json")); err == nil {
		if !flashpack.StampStale(extracted, info.Checksum) {
			fp, err := flashpack.ResolveRecovery(cacheDir, ref)
			return fp, true, err
		}
		detail("cached build is outdated — refreshing")
		flashpack.Purge(extracted, tarball)
	}

	cached := true
	if _, err := os.Stat(tarball); err != nil {
		cached = false
		if err := downloadRecoveryTarball(info, tarball, detail); err != nil {
			return nil, false, err
		}
	} else {
		detail("verifying cached recovery download")
		if err := verifySHA256(tarball, info.Checksum); err != nil {
			// A leftover tarball that no longer matches the manifest is a stale or
			// interrupted download; replace it rather than failing the flash.
			cached = false
			_ = os.Remove(tarball)
			if err := downloadRecoveryTarball(info, tarball, detail); err != nil {
				return nil, false, err
			}
		}
	}

	// The tarball on disk now matches the manifest. Stamp provenance before
	// extraction so even an interrupted extraction leaves a stale-detectable
	// cache. Best-effort: a missing stamp is grandfathered as current.
	_ = flashpack.WriteStamp(extracted, info.Checksum)

	detail("extracting and verifying every consumed file")
	// ResolveRecovery drops the .tar.zst once the extracted tree verifies, so a
	// version's on-disk footprint isn't doubled.
	fp, err := flashpack.ResolveRecovery(cacheDir, ref)
	if err != nil {
		return nil, cached, err
	}
	return fp, cached, nil
}

// downloadRecoveryTarball fetches info's artifact to tarball, verifying its
// checksum before it lands in the cache.
func downloadRecoveryTarball(info *recoveryFlashpackInfo, tarball string, detail func(string)) error {
	img := &imageInfo{DownloadURL: info.URL, ImageSize: info.SizeBytes, Version: info.Version}
	tmp, err := downloadImageInto(img, throttledDetail(detail, byteProgress))
	if err != nil {
		return fmt.Errorf("downloading recovery flashpack: %w", err)
	}
	detail("verifying download")
	if err := verifySHA256(tmp, info.Checksum); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, tarball); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("caching recovery flashpack: %w", err)
	}
	return nil
}

// prepareT234Workspace hard-links immutable partition images into a per-run
// directory and copies only the mutable config FAT image. The extracted cache
// tree is never opened for writing.
func prepareT234Workspace(fp *flashpack.Flashpack) (workspace, layoutPath string, err error) {
	src := fp.FlashImagesDir()
	workspace, _, err = prepareMutableWorkspace(src, fp.ConfigImage())
	if err != nil {
		return "", "", err
	}
	layoutRel, err := filepath.Rel(src, fp.PartitionLayout())
	if err != nil {
		_ = os.RemoveAll(workspace)
		return "", "", err
	}
	return workspace, filepath.Join(workspace, layoutRel), nil
}

func injectRecoveryConfig(imagePath string, creds []wendyconf.WifiCredential, deviceName string, provJSON []byte, out io.Writer, detail func(string)) error {
	detail("downloading agent")
	agentBinary, agentVer, _, err := resolveAgentArtifact("linux", "arm64", false)
	if err != nil {
		fmt.Fprintf(out, "warning: could not download wendy-agent (%v); using the agent baked into the image\n", err)
	} else {
		detail("agent " + agentVer)
	}
	d, err := diskfs.Open(imagePath)
	if err != nil {
		return fmt.Errorf("opening per-run config image: %w", err)
	}
	defer d.Close()
	fsys, err := d.GetFilesystem(0)
	if err != nil {
		return fmt.Errorf("reading config filesystem: %w", err)
	}
	return writeConfigFilesTo(fatWriter{fsys}, agentBinary, creds, deviceName, provJSON)
}

func t234RCMFiles(fp *flashpack.Flashpack) (order []string, memBCT, blob string, err error) {
	if len(fp.Manifest.RCMPhases) != 2 {
		return nil, "", "", fmt.Errorf("T234 flashpack must contain exactly two RCM phases")
	}
	for _, d := range fp.Manifest.RCMPhases[0] {
		order = append(order, filepath.FromSlash(d.File))
	}
	for _, d := range fp.Manifest.RCMPhases[1] {
		switch d.Type {
		case "bct_mem":
			memBCT = filepath.FromSlash(d.File)
		case "blob":
			blob = filepath.FromSlash(d.File)
		}
	}
	if len(order) == 0 || memBCT == "" || blob == "" {
		return nil, "", "", fmt.Errorf("T234 flashpack RCM phases omit bootROM files, bct_mem, or blob")
	}
	return order, memBCT, blob, nil
}

func saveOrinDeviceLogs(out io.Writer, logPath string, st *t234.FinalStatus) {
	if st == nil || len(st.Logs) == 0 {
		return
	}
	dir := strings.TrimSuffix(logPath, ".log") + "-device-logs"
	if logPath == "" {
		var err error
		if dir, err = os.MkdirTemp("", "orin-device-logs-"); err != nil {
			return
		}
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	names := make([]string, 0, len(st.Logs))
	for name := range st.Logs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		_ = os.WriteFile(filepath.Join(dir, filepath.Base(name)), st.Logs[name], 0o600)
	}
	fmt.Fprintf(out, "Device-side logs (%s): %s\n", strings.Join(names, ", "), dir)
}

func confirmOrinReady(opts t234InstallOptions) error {
	fmt.Println()
	fmt.Println(tui.Header("Recovering " + opts.DeviceName + " with WendyOS " + opts.Version))
	fmt.Println(orinRecoveryBriefingBox(opts))
	if note := orinMacFullDiskAccessNote(); note != "" {
		fmt.Println(note)
	}
	if note := orinWinSetupNote(); note != "" {
		fmt.Println(note)
	}
	if opts.Force {
		return nil
	}
	fmt.Println()
	ok, err := tui.Confirm("Is the target connected and in recovery mode?")
	if errors.Is(err, tui.ErrCancelled) || (err == nil && !ok) {
		return ErrUserCancelled
	}
	return err
}

// orinRecoveryMatch selects only recovery devices whose module family matches
// the install's device type: T234 modules share a chip but each SKU has its own
// recovery PID, and a jetson-orin-nano install must never pick up an AGX Orin
// sitting in recovery on the same host (or vice versa).
func orinRecoveryMatch(deviceType string) func(rcm.RecoveryDevice) bool {
	if deviceType == orinNanoDeviceType {
		return func(d rcm.RecoveryDevice) bool { return d.IsOrinNano() }
	}
	return func(d rcm.RecoveryDevice) bool { return d.IsOrinAGX() }
}

// orinRecoveryPort names the USB-C port carrying recovery/device mode for the
// given T234 device type. The Orin Nano devkit has a single USB-C port; the AGX
// Orin devkit's recovery port is the USB-C next to the 40-pin header.
func orinRecoveryPort(deviceType string) string {
	if deviceType == orinNanoDeviceType {
		return "the devkit's USB-C port"
	}
	return "the USB-C port next to the 40-pin header"
}

// orinRecoveryHints is the USB-recovery wait-UI text for a T234 Orin target,
// mirroring the cabling and recovery-entry guidance in orinRecoveryBriefingBox
// so the shared wait UI never shows the wrong board's port or buttons. The Orin
// Nano devkit has no buttons at all — recovery mode is entered by jumpering
// FC REC to GND — so it must never get AGX's button sequence.
func orinRecoveryHints(opts t234InstallOptions) recoveryWaitHints {
	hints := recoveryWaitHints{
		label:       opts.DeviceName,
		cablingLine: "the USB-C cable is in " + briefPort.Render(orinRecoveryPort(opts.DeviceType)),
		buttonLine:  "the recovery sequence: power off, hold " + briefKey.Render("Force Recovery") + ", tap " + briefKey.Render("Reset/Power") + ", release Force Recovery",
	}
	if opts.DeviceType == orinNanoDeviceType {
		hints.buttonLine = "the recovery jumper: " + briefKey.Render("FC REC") + " bridged to " + briefKey.Render("GND") + " (pins 9–10 on the button header under the module) when power is connected"
	}
	return hints
}

func orinRecoveryBriefingBox(opts t234InstallOptions) string {
	lines := []string{
		briefMarker.Render("●") + " " + briefTitle.Render("Destructive full recovery"),
		"  QSPI and " + briefKey.Render(strings.ToUpper(opts.Storage)) + " are updated together; all partitions, including /data, are erased.",
		"",
		briefMarker.Render("●") + " " + briefTitle.Render("USB-C cabling"),
		"  Connect this computer to " + briefPort.Render(orinRecoveryPort(opts.DeviceType)) + ".",
		"",
		briefMarker.Render("●") + " " + briefTitle.Render("Entering recovery mode"),
	}
	if opts.DeviceType == orinNanoDeviceType {
		lines = append(lines,
			"    "+briefNum.Render("1.")+" Disconnect power from the devkit (it has no buttons).",
			"    "+briefNum.Render("2.")+" Bridge "+briefKey.Render("FC REC")+" to "+briefKey.Render("GND")+" — pins 9–10 on the button header under the module — with a jumper.",
			"    "+briefNum.Render("3.")+" Connect power; the devkit boots straight into recovery mode. The jumper can then be removed.",
			"    "+briefNum.Render("4.")+" Keep the recovery USB-C cable attached until final SUCCESS.",
		)
	} else {
		lines = append(lines,
			"    "+briefNum.Render("1.")+" Power off the devkit and connect power.",
			"    "+briefNum.Render("2.")+" Hold "+briefKey.Render("Force Recovery")+", tap "+briefKey.Render("Reset/Power")+", then release Force Recovery.",
			"    "+briefNum.Render("3.")+" Keep the recovery USB-C cable attached until final SUCCESS.",
		)
	}
	if runtime.GOOS == "darwin" {
		lines = append(lines,
			"",
			briefMarker.Render("●")+" "+briefTitle.Render("macOS disk prompts"),
			"  wendy keeps macOS from asking about the Jetson's flashing disks. If it still says a",
			"  disk is not readable, choose "+briefKey.Render("Ignore")+"; "+briefKey.Render("Eject")+" interrupts the flash.",
		)
	}
	if runtime.GOOS == "windows" {
		lines = append(lines,
			"",
			briefMarker.Render("●")+" "+briefTitle.Render("Windows disk prompts"),
			"  While flashing, Windows may ask to format a newly attached disk —",
			"  the Jetson's raw flashing disks are expected to look unformatted. Choose "+briefKey.Render("Cancel")+";",
			"  formatting or ejecting them can corrupt or interrupt the flash.",
		)
	}
	return briefBorder.Render(strings.Join(lines, "\n"))
}

// orinMacFullDiskAccessNote returns a short note (macOS only) warning that a USB
// recovery flash writes directly to the Jetson's disk, which macOS gates behind
// Full Disk Access for the terminal. Mirrors the Windows WinUSB-driver note.
// Empty string on other platforms.
func orinMacFullDiskAccessNote() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	lines := []string{
		briefMarker.Render("●") + " " + briefTitle.Render("First-time setup: Full Disk Access"),
		"  Recovery writes directly to the Jetson's disk over USB, which macOS gates",
		"  behind " + briefKey.Render("Full Disk Access") + " for your terminal. If macOS asks, choose " + briefKey.Render("Allow") + ".",
		"  To pre-grant: " + briefPort.Render("System Settings ▸ Privacy & Security ▸ Full Disk Access") + ",",
		"  enable your terminal (e.g. " + briefKey.Render("Terminal.app") + "), then quit and reopen it.",
	}
	return briefBorder.Render(strings.Join(lines, "\n"))
}

// orinWinSetupNote returns a short note (Windows only) covering the two
// pieces of one-time host setup a recovery flash needs: the Jetson WinUSB
// driver install and the administrator (UAC) elevation for raw disk writes —
// a single consent prompt covers both, and accepting it continues the
// command in a new elevated console window. Mirrors thorWindowsDriverNote.
// Empty string on other platforms.
func orinWinSetupNote() string {
	// After the early UAC handoff (elevateForT234Flash) this process is
	// already elevated and the "expect a UAC prompt" guidance would be stale.
	if runtime.GOOS != "windows" || processElevated() {
		return ""
	}
	lines := []string{
		briefMarker.Render("●") + " " + briefTitle.Render("First-time setup: driver + administrator approval"),
		"  To talk to the Jetson over USB, Wendy installs a small " + briefKey.Render("WinUSB driver") + " for it",
		"  (one-time per computer), and recovery writes to the Jetson's disk require",
		"  " + briefKey.Render("administrator approval") + ". Expect a single UAC prompt; accepting it continues",
		"  this command in a new elevated console window.",
	}
	return briefBorder.Render(strings.Join(lines, "\n"))
}

// isMacRawDiskPermissionError reports a macOS raw-disk open denied by TCC: the
// sudo'd helper is root, but macOS still returns EPERM ("operation not
// permitted") unless the terminal has Full Disk Access.
func isMacRawDiskPermissionError(err error) bool {
	return runtime.GOOS == "darwin" && err != nil && strings.Contains(err.Error(), "operation not permitted")
}

// printOrinWinDiskAccessHint explains a blocked raw-disk write on Windows and
// what usually causes it. isWinRawDiskAccessError (per-OS, see
// os_install_orin_windows.go) decides when it prints.
func printOrinWinDiskAccessHint(w io.Writer) {
	body := strings.Join([]string{
		thorHintTitle.Render("⚠  Windows blocked access to the recovery disk"),
		"",
		"Windows refused the raw-disk access. Usual causes:",
		"",
		"  1. Another program is holding the disk — antivirus, backup, or disk tools.",
		"     Close or pause them and retry.",
		"  2. A Windows prompt to format the Jetson's disk was accepted or is pending.",
		"     Always choose " + thorHintEmph.Render("Cancel") + " on those prompts.",
		"",
		"Re-enter recovery mode on the Jetson and re-run the flash to retry.",
	}, "\n")
	fmt.Fprintln(w, "\n"+thorHintBorder.Render(body))
}

// printOrinFullDiskAccessHint explains the raw-disk permission failure and how
// to grant Full Disk Access. macOS-specific; root/sudo does not bypass TCC.
func printOrinFullDiskAccessHint(w io.Writer) {
	body := strings.Join([]string{
		thorHintTitle.Render("⚠  Permission denied opening the recovery disk"),
		"",
		"macOS blocked raw disk access — your terminal needs " + thorHintEmph.Render("Full Disk Access") + ".",
		"Running under sudo isn't enough, and macOS won't pop a prompt for it.",
		"",
		"  1. Open " + thorHintCmd.Render("System Settings ▸ Privacy & Security ▸ Full Disk Access") + ".",
		"  2. Enable your terminal (e.g. " + thorHintEmph.Render("Terminal.app") + "); use " + thorHintEmph.Render("+") + " to add it if it's missing.",
		"  3. Fully quit and reopen the terminal, then re-run the flash.",
	}, "\n")
	fmt.Fprintln(w, "\n"+thorHintBorder.Render(body))
}

func printOrinBadStateHint(w io.Writer, opts t234InstallOptions) {
	cmd := fmt.Sprintf("wendy os install --device-type %s --storage %s", opts.DeviceType, opts.Storage)
	body := strings.Join([]string{
		thorHintTitle.Render("⚠  Recovery was interrupted — the Jetson may not boot"), "",
		"QSPI or the rootfs may be partially written. Re-enter Force Recovery mode and retry:",
		thorHintCmd.Render(cmd),
	}, "\n")
	fmt.Fprintln(w, "\n"+thorHintBorder.Render(body))
}
