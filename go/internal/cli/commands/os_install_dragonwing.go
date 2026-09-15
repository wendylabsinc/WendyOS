package commands

// Dragonwing IQ-8275 flashing over EDL: resolve the qcomflash bundle from the
// manifest, download and extract it, then drive Sahara + Firehose in-process.
// Mirrors the Thor flow (plan → brief → confirm → pick device → step list), with
// two differences that come from the hardware: EDL is entered with a DIP switch
// rather than a button, and the bundle's descriptor skips the config and data
// partitions, so there is no config image to seed.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/archive"
	"github.com/wendylabsinc/wendy/go/internal/cli/qdl"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// dragonwingProgrammer is the Firehose programmer inside the bundle. The bundle
// also ships prog_firehose_lite.elf, which targets the SPI-NOR part.
const dragonwingProgrammer = "prog_firehose_ddr.elf"

// errDragonwingNothingWritten marks a failure before the first program command
// reached the device, so storage is untouched and warning that the board may
// not boot would be a false alarm.
var errDragonwingNothingWritten = errors.New("flash did not reach the device's storage")

// dragonwingExtractedFactor estimates the extracted tree from the compressed
// bundle. Partition images are stored at their full partition size and are
// mostly zeros, so the tarball expands enormously: a 601 MB 0.19.1 bundle
// extracts to ~12.5 GiB. Measured at ~21x; 23 keeps the estimate just above
// the real peak (tarball plus extracted tree).
const dragonwingExtractedFactor = 23.0

// dragonwingPlan is the resolved download/cache decision for one flash.
type dragonwingPlan struct {
	version string
	cached  bool // the extracted bundle is already on disk
	offline bool // the manifest was unreachable; a cached bundle is being reused
	dir     string
	tarball string
	info    *dragonwingBundleInfo
}

// planDragonwingBundle decides whether to reuse the cache or download.
//
// The cache is keyed on the published checksum as well as the version, because
// a --pr build keeps a stable "pr-N" tag across re-pushes: keying on the
// version alone would silently flash a stale build.
func planDragonwingBundle(cacheDir, version string, nightly bool, pr int) (dragonwingPlan, error) {
	info, err := getDragonwingBundleInfo(version, nightly, pr)
	if err != nil {
		// Only when the manifest could not be fetched: a manifest that simply
		// no longer lists the version (a closed PR, a withdrawn build) must
		// not silently flash the stale copy we happen to have.
		if errors.Is(err, ErrManifestUnreachable) {
			if v := offlineDragonwingVersion(version, pr); v != "" {
				if dir, ok := findCachedDragonwingBundle(cacheDir, v); ok {
					return dragonwingPlan{version: v, cached: true, offline: true, dir: dir}, nil
				}
			}
			if version != "" {
				return dragonwingPlan{}, fmt.Errorf("bundle %s not in cache and the manifest is unreachable: %w", version, err)
			}
		}
		return dragonwingPlan{}, err
	}
	if info.Checksum == "" {
		return dragonwingPlan{}, fmt.Errorf(
			"manifest entry for %s has no checksum — refusing to install an unverifiable download", info.Version)
	}
	if err := checkCacheSegment("version", info.Version); err != nil {
		return dragonwingPlan{}, err
	}
	if err := checkCacheSegment("checksum", info.Checksum); err != nil {
		return dragonwingPlan{}, err
	}

	dir := filepath.Join(dragonwingVersionCacheDir(cacheDir, info.Version), shortChecksum(info.Checksum))
	return dragonwingPlan{
		version: info.Version,
		cached:  bundleExtracted(dir),
		dir:     dir,
		tarball: filepath.Join(dir, "bundle.tar.gz"),
		info:    info,
	}, nil
}

// cacheSegment is what a manifest value must look like to be used as a
// directory name.
var cacheSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// checkCacheSegment rejects a manifest value that cannot be a path segment.
// Versions and checksums come from a downloaded manifest, so one containing a
// separator or ".." would place the cache — and the os.RemoveAll that prunes
// it — outside the cache directory.
func checkCacheSegment(kind, v string) error {
	if !cacheSegment.MatchString(v) || strings.Trim(v, ".") == "" {
		return fmt.Errorf("manifest %s %q cannot be used as a cache directory name", kind, v)
	}
	return nil
}

// dragonwingVersionCacheDir holds one directory per artifact of a version, so a
// version tag that reuses a name cannot collide with a different build of it.
func dragonwingVersionCacheDir(cacheDir, version string) string {
	return filepath.Join(cacheDir, dragonwingDeviceType, version)
}

func shortChecksum(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

func bundleExtracted(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "extracted"))
	return err == nil
}

// findCachedDragonwingBundle locates any cached bundle for version, used on the
// offline path where no checksum is available to pick between them.
func findCachedDragonwingBundle(cacheDir, version string) (string, bool) {
	if err := checkCacheSegment("version", version); err != nil {
		return "", false
	}
	entries, err := os.ReadDir(dragonwingVersionCacheDir(cacheDir, version))
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		dir := filepath.Join(dragonwingVersionCacheDir(cacheDir, version), e.Name())
		if e.IsDir() && bundleExtracted(dir) {
			return dir, true
		}
	}
	return "", false
}

// pruneStaleDragonwingBundles drops every cached artifact of a version except
// keep, so a re-pushed PR build leaves nothing behind. Best-effort: a failure
// here only wastes disk.
//
// Call it only once a replacement is extracted and usable — pruning earlier
// destroys the copy the offline path would otherwise fall back to.
func pruneStaleDragonwingBundles(versionRoot, keep string) {
	entries, err := os.ReadDir(versionRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if dir := filepath.Join(versionRoot, e.Name()); dir != keep {
			_ = os.RemoveAll(dir)
		}
	}
}

// offlineDragonwingVersion names the cached bundle to try when the manifest is
// unreachable: an explicit --version, or a PR's conventional "pr-N" tag.
func offlineDragonwingVersion(version string, pr int) string {
	if version != "" {
		return version
	}
	if pr > 0 {
		return fmt.Sprintf("pr-%d", pr)
	}
	return ""
}

// checkDragonwingDiskSpace fails early when the cache volume is too small.
// Best-effort: an unknown size never blocks.
func checkDragonwingDiskSpace(cacheDir string, plan dragonwingPlan) error {
	if plan.cached || plan.info == nil || plan.info.SizeBytes <= 0 {
		return nil
	}
	// Only the extraction is left when the tarball is already cached.
	factor := 1 + dragonwingExtractedFactor
	if _, err := os.Stat(plan.tarball); err == nil {
		factor = dragonwingExtractedFactor
	}
	needed := int64(float64(plan.info.SizeBytes) * factor)
	avail, ok := diskAvailBytes(cacheDir)
	if !ok || avail >= needed {
		return nil
	}
	const gib = 1 << 30
	return fmt.Errorf(
		"not enough free disk space for WendyOS %s: needs about %.1f GiB in %s, but only %.1f GiB is free.\nFree up space and try again",
		plan.version, float64(needed)/gib, cacheDir, float64(avail)/gib)
}

// downloadAndExtractDragonwingBundle resolves the bundle to the directory
// holding its flash descriptors, downloading and verifying it if not cached.
func downloadAndExtractDragonwingBundle(plan dragonwingPlan, detail func(string)) (string, bool, error) {
	extracted := filepath.Join(plan.dir, "extracted")
	if plan.cached {
		dir, err := qdl.ResolveBundleDir(extracted)
		return dir, true, err
	}

	if _, err := os.Stat(plan.tarball); err != nil {
		img := &imageInfo{DownloadURL: plan.info.URL, ImageSize: plan.info.SizeBytes, Version: plan.version}
		tmp, err := downloadImageInto(img, throttledDetail(detail, byteProgress))
		if err != nil {
			return "", false, fmt.Errorf("downloading flash bundle: %w", err)
		}
		detail("verifying")
		if err := verifySHA256(tmp, plan.info.Checksum); err != nil {
			os.Remove(tmp)
			return "", false, err
		}
		if err := os.MkdirAll(filepath.Dir(plan.tarball), 0o755); err != nil {
			os.Remove(tmp)
			return "", false, err
		}
		if err := os.Rename(tmp, plan.tarball); err != nil {
			os.Remove(tmp)
			return "", false, fmt.Errorf("caching flash bundle: %w", err)
		}
	}

	detail("extracting")
	if err := archive.ExtractTarGz(plan.tarball, extracted); err != nil {
		return "", false, fmt.Errorf("extracting flash bundle: %w", err)
	}
	dir, err := qdl.ResolveBundleDir(extracted)
	if err != nil {
		// Leaving it would make plan.cached true forever, short-circuiting the
		// download and repeating this error on every retry.
		_ = os.RemoveAll(extracted)
		return "", false, err
	}
	// Only now is the tree known good: reclaim the tarball and drop older
	// artifacts of the same version.
	_ = os.Remove(plan.tarball)
	pruneStaleDragonwingBundles(filepath.Dir(plan.dir), plan.dir)
	return dir, false, err
}

// installDragonwing flashes an IQ-8275 over EDL.
func installDragonwing(ctx context.Context, version string, nightly, force bool, prNumber int) error {
	if !qdl.Supported() {
		return errors.New("flashing a Dragonwing over EDL is not supported on this platform")
	}
	cacheDir, err := osCacheDir()
	if err != nil {
		return fmt.Errorf("resolving cache dir: %w", err)
	}

	plan, err := planDragonwingBundle(cacheDir, version, nightly, prNumber)
	if err != nil {
		return err
	}
	if plan.offline {
		fmt.Println(tui.WarningMessage(fmt.Sprintf(
			"Offline — using cached WendyOS %s; cannot confirm it is the latest build.", plan.version)))
	}
	if err := checkDragonwingDiskSpace(cacheDir, plan); err != nil {
		return err
	}

	if err := confirmDragonwingReady(plan.version, force); err != nil {
		return err
	}

	dev, err := pickDragonwingEDLDevice()
	if err != nil {
		if isEDLAccessErr(err) {
			fmt.Println("\n" + dragonwingUSBAccessHint())
		}
		return err
	}
	fmt.Printf("\n%s %s\n", tui.Dim("Target:"), tui.Device(dragonwingTargetLabel(dev)))
	if !force {
		fmt.Println(tui.Dim("  Only one Qualcomm device should be in EDL mode: the id 05c6:9008 is"))
		fmt.Println(tui.Dim("  generic, so wendy cannot confirm this is an IQ-8275."))
	}

	if !force {
		fmt.Println()
		fmt.Println(tui.WarningMessage("This rewrites both OS slots on the board's UFS. /data is preserved."))
		ok, err := tui.ConfirmNoDefaultDanger(
			fmt.Sprintf("Write the Dragonwing IQ-8275 bundle to %s?", dragonwingTargetLabel(dev)))
		if errors.Is(err, tui.ErrCancelled) || (err == nil && !ok) {
			return ErrUserCancelled
		}
		if err != nil {
			return err
		}
	}

	if err := prepareDragonwingHost(dev); err != nil {
		return err
	}

	flashCtx, cancelFlash := context.WithCancel(ctx)
	defer cancelFlash()

	// Keep a durable log of the whole flash for post-mortems. Best-effort: a
	// log we cannot open never blocks the flash.
	var logW io.Writer = io.Discard
	if dir, derr := config.LogDir(); derr == nil {
		logPath := filepath.Join(dir, "dragonwing-flash-"+time.Now().Format("20060102-150405")+".log")
		if lf, lerr := os.Create(logPath); lerr == nil {
			defer lf.Close() //nolint:errcheck
			fmt.Fprintf(lf, "wendy os install — Dragonwing IQ-8275 — WendyOS %s\n\n", plan.version)
			logW = lf
			defer func() { fmt.Println(tui.Dim("Full flash log: " + logPath)) }()
		}
	}

	var bundleDir string
	steps := []flashStep{
		{id: stepDownload, label: "Download flash bundle", run: func(_ io.Writer, detail func(string)) (bool, error) {
			dir, cached, err := downloadAndExtractDragonwingBundle(plan, detail)
			bundleDir = dir
			return cached, err
		}},
		{id: stepFlashPartitions, label: "Flash partitions",
			abortWarning: "Partitions are being written — aborting now can leave the board unbootable. Press ctrl+c again to abort anyway.",
			run: func(out io.Writer, detail func(string)) (bool, error) {
				return false, flashDragonwing(flashCtx, bundleDir, dev, out, detail)
			}},
	}

	failedID, err := runFlashSteps(fmt.Sprintf("Flashing WendyOS %s", plan.version), steps, cancelFlash, logW)
	if err != nil {
		switch {
		case isEDLAccessErr(err):
			// Access can be refused when claiming the interface, not only
			// during the scan, so the remedy has to be reachable here too.
			fmt.Println("\n" + dragonwingUSBAccessHint())
		case errors.Is(err, errDragonwingNothingWritten):
			// The board never got a write command, so it still holds whatever
			// it held before. Say so, instead of implying it is now broken —
			// and say what to do about DIP switch 3, which is still ON.
			fmt.Println()
			fmt.Println(tui.WarningMessage("Nothing was written — the board is unchanged and still boots what it had."))
			fmt.Println("  Set " + briefKey.Render("DIP switch 3") + " back to " + briefKey.Render("OFF") +
				" and power-cycle, or leave it ON to retry.")
		case failedID == stepFlashPartitions:
			printDragonwingBadStateHint(os.Stdout)
		}
		if errors.Is(err, tui.ErrCancelled) {
			return ErrUserCancelled
		}
		return err
	}

	fmt.Println(tui.SuccessMessage(fmt.Sprintf("Flashed WendyOS %s.", plan.version)))
	fmt.Println()
	fmt.Println("  Now set " + briefKey.Render("DIP switch 3") + " back to " + briefKey.Render("OFF") +
		" and power-cycle the board.")
	fmt.Println("  " + briefDim.Render("Left ON, it will boot into EDL again instead of WendyOS."))
	return nil
}

// flashDragonwing hands the programmer over with Sahara, then programs every
// partition and applies the GPT patches. No reset is sent: EDL was entered with
// a latching DIP switch, so a reset would only land the board back in EDL.
func flashDragonwing(ctx context.Context, bundleDir string, dev qdl.DeviceInfo, out io.Writer, detail func(string)) error {
	flash, err := qdl.LoadFlashPlan(bundleDir)
	if err != nil {
		return errors.Join(err, errDragonwingNothingWritten)
	}
	prog, err := os.ReadFile(filepath.Join(bundleDir, dragonwingProgrammer))
	if err != nil {
		return errors.Join(fmt.Errorf("reading the Firehose programmer from the bundle: %w", err),
			errDragonwingNothingWritten)
	}

	conn, err := qdl.Open(dev)
	if err != nil {
		return errors.Join(err, errDragonwingNothingWritten)
	}
	defer conn.Close() //nolint:errcheck

	detail("uploading programmer")
	err = qdl.UploadProgrammer(conn, dragonwingProgrammer, prog, nil)
	if err != nil {
		return errors.Join(err, errDragonwingNothingWritten)
	}

	session := qdl.NewSession(conn, func(line string) { fmt.Fprintln(out, "device: "+line) })
	detail("starting programmer")
	if err := session.Configure(qdl.StorageUFS); err != nil {
		return errors.Join(err, errDragonwingNothingWritten)
	}

	for _, e := range flash.Programs {
		label := e.Label
		err := session.Program(ctx, flash.Dir, e, throttledDetail(detail,
			func(done, total int64) string {
				return fmt.Sprintf("%q · %d%%", label, percent(done, total))
			}))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote %q\n", label)
	}

	// The descriptor's GPT covers only the partitions it declares; these
	// patches stretch the last one over the rest of the disk.
	detail("updating partition table")
	for _, p := range flash.Patches {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := session.Patch(p); err != nil {
			return err
		}
	}
	return nil
}

// percent caps at 99: the device only acknowledges a partition after the last
// chunk, so 100% would be claimed before the write is known to have landed.
func percent(done, total int64) int {
	if total <= 0 {
		return 0
	}
	return int(min(done*100/total, 99))
}

// printDragonwingBadStateHint explains how to recover when a flash failed part
// way through, which can leave the board unbootable.
func printDragonwingBadStateHint(w io.Writer) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, tui.WarningMessage("The flash did not finish — the board may not boot."))
	fmt.Fprintln(w, tui.Dim("  Leave "+briefKey.Render("DIP switch 3")+" set to ON, power-cycle the board to"))
	fmt.Fprintln(w, tui.Dim("  re-enter EDL, and run the same command again."))
}

// pickDragonwingEDLDevice finds the board in EDL mode, waiting for it to appear
// if it is not there yet, and asks which one when several are attached.
func pickDragonwingEDLDevice() (qdl.DeviceInfo, error) {
	devices, err := qdl.List()
	if err != nil {
		return qdl.DeviceInfo{}, err
	}
	if len(devices) == 0 {
		devices, err = waitForRecovery(dragonwingEDLHints(), qdl.List)
		if err != nil {
			return qdl.DeviceInfo{}, err
		}
	}
	if len(devices) == 1 {
		return devices[0], nil
	}

	// Several boards in EDL: flashing the wrong one is unrecoverable from
	// here, so never guess.
	if !isInteractiveTerminal() {
		return qdl.DeviceInfo{}, fmt.Errorf(
			"%d boards are in EDL mode; disconnect all but the one to flash", len(devices))
	}
	// Key on the USB address, not the serial: a board that reports no serial
	// would give every row the same empty value and the choice would be lost.
	items := make([]tui.PickerItem, 0, len(devices))
	byAddress := make(map[string]qdl.DeviceInfo, len(devices))
	for _, d := range devices {
		key := d.Key()
		byAddress[key] = d
		items = append(items, tui.PickerItem{
			Name:        dragonwingTargetLabel(d),
			Description: d.String(),
			Value:       key,
		})
	}
	key, err := pickFromItems("Select the board to flash", items)
	if err != nil {
		return qdl.DeviceInfo{}, err
	}
	dev, ok := byAddress[key]
	if !ok {
		return qdl.DeviceInfo{}, fmt.Errorf("selected board %q is no longer attached", key)
	}
	return dev, nil
}

// rejectDragonwingSeedFlags refuses the flags that pre-seed a device through its
// config partition. The descriptor declares config and data with an empty
// filename so the programmer skips them — which is why a reflash keeps identity
// and saved Wi-Fi, and why there is no config image to write into.
func rejectDragonwingSeedFlags(wifi wifiCLIOptions, deviceName string, preOpts preEnrollOptions) error {
	var flags []string
	if wifi.SSID != "" || wifi.Password != "" || len(wifi.Entries) > 0 {
		flags = append(flags, "--wifi/--wifi-ssid")
	}
	if deviceName != "" {
		flags = append(flags, "--device-name")
	}
	// Only an explicit request is an error: the default mode means "prompt if
	// it makes sense", and here it does not.
	if preOpts.mode == preEnrollForced {
		flags = append(flags, "--pre-enroll")
	}
	// Only meaningful alongside pre-enrollment, so on its own it would be
	// accepted and quietly ignored.
	if preOpts.cloudGRPC != "" {
		flags = append(flags, "--cloud-grpc")
	}
	if len(flags) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%s cannot be applied when flashing a Dragonwing: the flash preserves the existing config and data partitions, so there is nothing to seed.\nConfigure the board after it boots (wendy device wifi, wendy device rename)",
		strings.Join(flags, ", "))
}

// checkDragonwingFlags rejects the flags that do not apply to an EDL flash.
func checkDragonwingFlags(rootfsOnly bool, drive string, noBmap, overwriteInternal bool,
	storageOverride string, wifi wifiCLIOptions, deviceName string, preOpts preEnrollOptions) error {
	if rootfsOnly {
		return fmt.Errorf("--rootfs-only is not available for the Dragonwing IQ-8275")
	}
	// Not rejectRecoveryDriveFlags: its message is about choosing storage on a
	// Jetson over USB, which means nothing here.
	var drivish []string
	if drive != "" {
		drivish = append(drivish, "--drive")
	}
	if noBmap {
		drivish = append(drivish, "--no-bmap")
	}
	if overwriteInternal {
		drivish = append(drivish, "--yes-overwrite-internal")
	}
	if len(drivish) > 0 {
		verb := "does not"
		if len(drivish) > 1 {
			verb = "do not"
		}
		return fmt.Errorf("%s %s apply to the Dragonwing IQ-8275: it is flashed over EDL, and the partition layout comes from the bundle rather than a host drive",
			strings.Join(drivish, ", "), verb)
	}
	if storageOverride != "" {
		return fmt.Errorf("--storage does not apply to the Dragonwing IQ-8275: it flashes its onboard UFS")
	}
	return rejectDragonwingSeedFlags(wifi, deviceName, preOpts)
}

// dragonwingTargetLabel describes the device about to be written to.
//
// It deliberately does not claim the device *is* an IQ-8275: 05c6:9008 is the
// generic Qualcomm EDL id, shared by every Qualcomm phone, modem and dev
// board, and nothing here identifies the SoC. So it reports the serial, CID and
// USB address — enough to tell two attached devices apart — and leaves the
// board name attached to the bundle, which is the part we do know.
func dragonwingTargetLabel(dev qdl.DeviceInfo) string {
	return dev.String()
}

// edlUdevRule grants access to a Qualcomm SoC in EDL mode. The vendor-wide
// match mirrors the Jetson rule: 05c6 devices expose this interface only while
// in EDL, so the grant is bounded.
const edlUdevRule = `SUBSYSTEM=="usb", ATTRS{idVendor}=="05c6", MODE="0660", GROUP="plugdev", TAG+="uaccess"`

const edlUdevRulePath = "/etc/udev/rules.d/70-wendy-qualcomm.rules"

// isEDLAccessErr reports whether the OS refused access to the device rather
// than there being none attached.
func isEDLAccessErr(err error) bool {
	return errors.Is(err, qdl.ErrUSBAccess)
}

// dragonwingUSBAccessHint explains how to grant access, which otherwise
// surfaces only as a raw libusb error code.
func dragonwingUSBAccessHint() string {
	lines := []string{
		briefTitle.Render("\u26a0  USB access denied"),
		"",
		"The board is in EDL mode, but the OS refused wendy access to its USB device.",
	}
	if runtime.GOOS == "linux" {
		lines = append(lines,
			"Grant access with a udev rule (one-time):",
			"",
			briefDim.Render("  echo '"+edlUdevRule+"' \\"),
			briefDim.Render("    | sudo tee "+edlUdevRulePath),
			briefDim.Render("  sudo udevadm control --reload-rules && sudo udevadm trigger"),
			"",
			"Add your user to the "+briefKey.Render("plugdev")+" group, replug the cable and retry,",
			"or re-run the flash with sudo.",
		)
	} else if runtime.GOOS == "windows" {
		lines = append(lines, "Run the install command again to install or repair the Qualcomm EDL WinUSB binding.",
			"Accept the administrator prompt, and close other flashing tools that may have the board open.")
	} else {
		lines = append(lines, "Re-run the flash with sudo so wendy can claim the device.")
	}
	return briefBorder.Render(strings.Join(lines, "\n"))
}
