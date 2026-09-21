package commands

// Dragonwing flashing over EDL: resolve the qcomflash bundle from the
// manifest, download and extract it, then drive Sahara + Firehose in-process.
// Mirrors the Thor flow (plan → brief → confirm → pick device → step list), with
// two differences that come from the hardware: EDL is entered with a DIP switch
// rather than a button, and the bundle ships no config image, so the host builds
// one to seed (os_install_dragonwing_config.go). A flash is a factory reset: the
// data filesystem is blanked so first boot recreates it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/archive"
	"github.com/wendylabsinc/wendy/go/internal/cli/qdl"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

// dragonwingProgrammer is the Firehose programmer inside the bundle. The bundle
// also ships prog_firehose_lite.elf, which targets the SPI-NOR part.
const dragonwingProgrammer = "prog_firehose_ddr.elf"

// dragonwingBoard is one EDL-flashed Qualcomm board. msmID is the only way to
// tell them apart: they share the generic 05c6:9008 EDL id.
type dragonwingBoard struct {
	deviceType string
	msmID      uint32
}

// dragonwingDeviceTypePrefix is what the publisher keys the EDL bundle on, so
// the safety filter matches on it rather than on the registry below: a new
// board is published before wendy learns to flash it.
const dragonwingDeviceTypePrefix = "dragonwing-"

var dragonwingBoards = []dragonwingBoard{
	{deviceType: "dragonwing-iq-8275", msmID: 0x002e70e1},
	{deviceType: "dragonwing-iq-9075", msmID: 0x002eb0e1},
}

func dragonwingBoardFor(deviceType string) (dragonwingBoard, bool) {
	for _, b := range dragonwingBoards {
		if b.deviceType == deviceType {
			return b, true
		}
	}
	return dragonwingBoard{}, false
}

// verifyDragonwingBoard is a safeguard, not a gate: only a chip id claimed by
// another registered board is certain enough to refuse. A quiet chip, or a
// stepping no table lists yet, comes back as a caution for the caller to show —
// refusing those would make good hardware unflashable.
//
// A refusal names bundleCache: the bundle for the board that was asked for is
// already extracted there, ~12 GiB of it, and nothing ever prunes another
// board's cache.
func verifyDragonwingBoard(board dragonwingBoard, got qdl.ChipID, readErr error, bundleCache string) (caution string, err error) {
	want := humanReadableDeviceType(board.deviceType)
	switch {
	case readErr != nil:
		return "Could not read the chip id, so wendy cannot confirm this is a " + want + ".", nil
	case got.MsmID == board.msmID:
		return "", nil
	}
	for _, other := range dragonwingBoards {
		if other.msmID == got.MsmID {
			mismatch := fmt.Errorf(
				"this board reports as a %s, but the install targets a %s — re-run with --device-type %s",
				humanReadableDeviceType(other.deviceType), want, other.deviceType)
			if bundleCache != "" {
				mismatch = fmt.Errorf("%w\nthe %s bundle for this run stays cached in %s and can be deleted",
					mismatch, want, bundleCache)
			}
			return "", errors.Join(mismatch, errDragonwingNothingWritten)
		}
	}
	return fmt.Sprintf("This board reports chip id %#x, which wendy cannot confirm is a %s.",
		got.MsmID, want), nil
}

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
func planDragonwingBundle(cacheDir string, board dragonwingBoard, version string, nightly bool, pr int) (dragonwingPlan, error) {
	info, err := getDragonwingBundleInfo(board, version, nightly, pr)
	if err != nil {
		// Only when the manifest could not be fetched: a manifest that simply
		// no longer lists the version (a closed PR, a withdrawn build) must
		// not silently flash the stale copy we happen to have.
		if errors.Is(err, ErrManifestUnreachable) {
			if v := offlineDragonwingVersion(version, pr); v != "" {
				if dir, ok := findCachedDragonwingBundle(cacheDir, board, v); ok {
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

	dir := filepath.Join(dragonwingVersionCacheDir(cacheDir, board, info.Version), shortChecksum(info.Checksum))
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
func dragonwingVersionCacheDir(cacheDir string, board dragonwingBoard, version string) string {
	return filepath.Join(cacheDir, board.deviceType, version)
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
func findCachedDragonwingBundle(cacheDir string, board dragonwingBoard, version string) (string, bool) {
	if err := checkCacheSegment("version", version); err != nil {
		return "", false
	}
	entries, err := os.ReadDir(dragonwingVersionCacheDir(cacheDir, board, version))
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		dir := filepath.Join(dragonwingVersionCacheDir(cacheDir, board, version), e.Name())
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
		if err != nil {
			// Leaving the tree in place keeps plan.cached short-circuiting the
			// download, so every later run fails here identically. The error
			// may be about the filesystem rather than the tree, so do not
			// claim a removal that did not happen.
			retry := "run the same command again"
			if plan.offline {
				// The tarball is reclaimed once a tree is known good, so
				// replacing it needs the manifest back.
				retry = "run the same command again once the manifest is reachable"
			}
			if rmErr := os.RemoveAll(extracted); rmErr != nil {
				return "", true, fmt.Errorf("the cached bundle in %s is unusable and could not be removed (%v) — delete that directory, then %s: %w",
					plan.dir, rmErr, retry, err)
			}
			return "", true, fmt.Errorf("the cached bundle in %s was unusable and has been discarded, so %s: %w",
				plan.dir, retry, err)
		}
		return dir, true, nil
	}

	if _, err := os.Stat(plan.tarball); err != nil {
		if plan.info == nil {
			// Only the offline plan leaves info nil, and that one is always
			// cached — this guards the derefs below rather than a live path.
			return "", false, errors.New("no bundle to download: the manifest was unreachable and no cached bundle was found")
		}
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
	// ExtractTarGz builds the tree in a temp sibling and renames it into place,
	// so "extracted" — which is itself the cache marker — only ever names a
	// complete tree, even when this run is killed mid-extraction.
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
	return dir, false, nil
}

// installDragonwing flashes a Dragonwing board over EDL.
func installDragonwing(ctx context.Context, board dragonwingBoard, version string, nightly, force bool, prNumber int,
	wifi wifiCLIOptions, deviceName string, preOpts preEnrollOptions) error {
	if !qdl.Supported() {
		return errors.New("flashing a Dragonwing over EDL is not supported on this platform")
	}
	cacheDir, err := osCacheDir()
	if err != nil {
		return fmt.Errorf("resolving cache dir: %w", err)
	}

	plan, err := planDragonwingBundle(cacheDir, board, version, nightly, prNumber)
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

	// Resolve what the flash will write up front, as Thor does: the prompts
	// must run before the flash UI takes over the terminal, and a bad flag
	// should abort before the board is touched.
	creds, err := resolveWiFiCredentialsList(wifi)
	if err != nil {
		return err
	}
	name, err := resolveDeviceName(deviceName)
	if err != nil {
		return err
	}
	provJSON, err := resolveProvisioningJSON(ctx, preOpts, name)
	if err != nil {
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
		fmt.Println()
		fmt.Println(tui.WarningMessage(
			"This rewrites the board's UFS: both OS slots, the config partition, and /data. Device identity, enrollment, saved Wi-Fi and app data are discarded — the board comes back as a new device."))
		fmt.Println(tui.Dim("  Every board in EDL reports the generic id 05c6:9008, so wendy confirms"))
		fmt.Println(tui.Dim("  the model from its chip id during the flash, not here."))
		ok, err := tui.ConfirmNoDefaultDanger(
			fmt.Sprintf("Write the %s bundle to %s?", humanReadableDeviceType(board.deviceType), dragonwingTargetLabel(dev)))
		if errors.Is(err, tui.ErrCancelled) || (err == nil && !ok) {
			return ErrUserCancelled
		}
		if err != nil {
			return err
		}
	}

	// On Windows this rebinds the board's driver for good, and the flash needs
	// the interface it claims, so neither may run before the go-ahead.
	if err := prepareDragonwingHost(dev); err != nil {
		return err
	}

	flashCtx, cancelFlash := context.WithCancel(ctx)
	defer cancelFlash()

	logW, closeFlashLog := openDragonwingFlashLog(board, plan.version)
	defer closeFlashLog()

	// The image holds the Wi-Fi PSK and any enrollment key in the clear, so it
	// lives only for the flash rather than in the bundle cache.
	seedDir, err := os.MkdirTemp("", "wendy-dragonwing-config-")
	if err != nil {
		return fmt.Errorf("creating a workspace for the config image: %w", err)
	}
	defer os.RemoveAll(seedDir) //nolint:errcheck

	zerosPath, err := writeDragonwingZeros(seedDir)
	if err != nil {
		return err
	}

	run := &dragonwingFlashRun{
		plan: plan, dev: dev, board: board,
		seedDir: seedDir, zerosPath: zerosPath,
		creds: creds, name: name, provJSON: provJSON,
	}
	failedID, err := runFlashSteps(fmt.Sprintf("Flashing WendyOS %s", plan.version),
		run.steps(flashCtx), cancelFlash, logW)
	return finishDragonwingFlash(os.Stdout, run.collected(), plan.version, err, failedID == stepFlashPartitions)
}

// dragonwingFlashRun holds what the flash steps share: the closures fill
// bundleDir then flash in order, and collect into warnings the output the steps
// UI withholds until it releases the terminal.
type dragonwingFlashRun struct {
	plan      dragonwingPlan
	dev       qdl.DeviceInfo
	board     dragonwingBoard
	seedDir   string
	zerosPath string
	creds     []wendyconf.WifiCredential
	name      string
	provJSON  []byte

	bundleDir string
	flash     *qdl.FlashPlan

	// The flash worker can outlive the steps UI on an abort, so it may still
	// be reporting a warning while the caller is printing what it collected.
	mu       sync.Mutex
	warnings []string
}

func (r *dragonwingFlashRun) warn(w string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warnings = append(r.warnings, w)
}

func (r *dragonwingFlashRun) collected() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.warnings)
}

// steps lists the flash in the order the UI runs it; ctx carries the user's
// abort, which only the partition writes can act on.
func (r *dragonwingFlashRun) steps(ctx context.Context) []flashStep {
	return []flashStep{
		{id: stepDownload, label: "Download flash bundle", run: func(_ io.Writer, detail func(string)) (bool, error) {
			dir, cached, err := downloadAndExtractDragonwingBundle(r.plan, detail)
			r.bundleDir = dir
			if err != nil {
				// Nothing is sent to the board until the next step, and the
				// user has already confirmed the destructive prompt by here.
				return cached, errors.Join(err, errDragonwingNothingWritten)
			}
			return cached, nil
		}},
		{id: stepProvision, label: "Write config partition", run: func(out io.Writer, detail func(string)) (bool, error) {
			// Everything the flash will write is resolved here, before the
			// first write command: a failure discovered mid-flash leaves the
			// board half-written with the GPT still unpatched.
			flash, err := planDragonwingFlash(r.seedDir, r.bundleDir, r.zerosPath,
				r.creds, r.name, r.provJSON, out, detail, r.warn)
			if err != nil {
				return false, errors.Join(err, errDragonwingNothingWritten)
			}
			r.flash = flash
			return false, nil
		}},
		{id: stepFlashPartitions, label: "Flash partitions",
			abortWarning: "Partitions are being written — aborting now can leave the board unbootable. Press ctrl+c again to abort anyway.",
			run: func(out io.Writer, detail func(string)) (bool, error) {
				return false, flashDragonwing(ctx, r.flash, r.dev, r.board, r.plan.dir, out, detail, r.warn)
			}},
	}
}

// finishDragonwingFlash prints what the steps UI withheld, then the outcome.
// The collected warnings print on both paths: a caution about which board
// answered explains a failure at least as often as it qualifies a success.
func finishDragonwingFlash(w io.Writer, warnings []string, version string, err error, reachedPartitions bool) error {
	for _, warning := range warnings {
		fmt.Fprintln(w, tui.WarningMessage(warning))
	}
	if err != nil {
		reportDragonwingFailure(w, err, reachedPartitions)
		if errors.Is(err, tui.ErrCancelled) {
			return ErrUserCancelled
		}
		return err
	}
	fmt.Fprintln(w, tui.SuccessMessage(fmt.Sprintf("Flashed WendyOS %s.", version)))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Now set "+briefKey.Render("DIP switch 3")+" back to "+briefKey.Render("OFF")+
		" and power-cycle the board.")
	fmt.Fprintln(w, "  "+briefDim.Render("Left ON, it will boot into EDL again instead of WendyOS."))
	return nil
}

// openDragonwingFlashLog keeps a durable log of the whole flash for
// post-mortems. Best-effort: a log that cannot be opened never blocks the
// flash. The returned close prints the path, so it belongs in a defer.
func openDragonwingFlashLog(board dragonwingBoard, version string) (io.Writer, func()) {
	dir, err := config.LogDir()
	if err != nil {
		return io.Discard, func() {}
	}
	logPath := filepath.Join(dir, "dragonwing-flash-"+time.Now().Format("20060102-150405")+".log")
	lf, err := os.Create(logPath)
	if err != nil {
		return io.Discard, func() {}
	}
	fmt.Fprintf(lf, "wendy os install — %s — WendyOS %s\n\n", humanReadableDeviceType(board.deviceType), version)
	return lf, func() {
		_ = lf.Close()
		fmt.Println(tui.Dim("Full flash log: " + logPath))
	}
}

// planDragonwingFlash resolves every write the flash will make: the seeded
// config image, and the zeros that blank /data. Non-fatal problems go to warn,
// which the caller surfaces once the steps UI has released the terminal.
func planDragonwingFlash(seedDir, bundleDir, zerosPath string, creds []wendyconf.WifiCredential, deviceName string, provJSON []byte,
	out io.Writer, detail func(string), warn func(string)) (*qdl.FlashPlan, error) {
	plan, err := qdl.LoadFlashPlan(bundleDir)
	if err != nil {
		return nil, err
	}

	agent, agentWarning := resolveSeedAgent(out, detail)
	if agentWarning != "" {
		warn(agentWarning)
	}
	img, err := buildDragonwingConfigImage(seedDir, plan, agent, creds, deviceName, provJSON)
	if err == nil {
		err = plan.Seed(dragonwingConfigLabel, img)
	}
	// Whatever config cannot be seeded with, it is blanked with: a flash is a
	// factory reset, and a surviving provisioning.json would re-enrol the fresh
	// install as the previous device.
	if err != nil {
		if provisioningRequired(creds, deviceName, provJSON) {
			return nil, err
		}
		// Worded for both outcomes: the warning is printed whether or not the
		// flash that follows it succeeds.
		warn(fmt.Sprintf("Could not write the config partition (%v) — it was blanked instead, so the board comes up unprovisioned.", err))
		if err := plan.Blank(dragonwingConfigLabel, zerosPath); err != nil {
			return nil, err
		}
	}

	if err := plan.Blank(dragonwingDataLabel, zerosPath); err != nil {
		return nil, err
	}
	return plan, nil
}

// flashDragonwing hands the programmer over with Sahara, then programs every
// partition and applies the GPT patches. No reset is sent: EDL was entered with
// a latching DIP switch, so a reset would only land the board back in EDL.
//
// A chip id that cannot confirm the board goes to warn rather than to the
// terminal: a step's own output is withheld unless the step fails, so the
// caller prints it once the steps UI is done.
func flashDragonwing(ctx context.Context, flash *qdl.FlashPlan, dev qdl.DeviceInfo, board dragonwingBoard,
	bundleCache string, out io.Writer, detail func(string), warn func(string)) error {
	prog, err := os.ReadFile(filepath.Join(flash.Dir, dragonwingProgrammer))
	if err != nil {
		return errors.Join(fmt.Errorf("reading the Firehose programmer from the bundle: %w", err),
			errDragonwingNothingWritten)
	}

	conn, err := qdl.Open(dev)
	if err != nil {
		return errors.Join(err, errDragonwingNothingWritten)
	}
	defer conn.Close() //nolint:errcheck

	// The chip id is read on this connection: the read leaves the Sahara
	// session in image-transfer mode, which is what the upload below needs.
	detail("identifying board")
	chip, chipErr := qdl.ReadChipID(conn)
	caution, err := verifyDragonwingBoard(board, chip, chipErr, bundleCache)
	if caution != "" {
		fmt.Fprintln(out, caution)
		warn(caution)
	}
	if err != nil {
		// Already tagged as nothing-written: only the programmer upload below
		// touches the board, and it has not run.
		return err
	}

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

// reportDragonwingFailure prints the remedy that fits the failure: granting USB
// access, a board nothing was written to, or one left part-written.
func reportDragonwingFailure(w io.Writer, err error, reachedPartitions bool) {
	switch {
	case isEDLAccessErr(err):
		// Access can be refused when claiming the interface, not only during
		// the scan, so the remedy has to be reachable here too.
		fmt.Fprintln(w, "\n"+dragonwingUSBAccessHint())
	case errors.Is(err, errDragonwingNothingWritten),
		errors.Is(err, tui.ErrCancelled) && !reachedPartitions:
		// The board never got a write command, so it still holds whatever it
		// held before. Say so, instead of implying it is now broken — and say
		// what to do about DIP switch 3, which is still ON.
		fmt.Fprintln(w)
		fmt.Fprintln(w, tui.WarningMessage("Nothing was written — the board is unchanged and still boots what it had."))
		fmt.Fprintln(w, "  Set "+briefKey.Render("DIP switch 3")+" back to "+briefKey.Render("OFF")+
			" and power-cycle, or leave it ON to retry.")
	case reachedPartitions:
		printDragonwingBadStateHint(w)
	}
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

// checkDragonwingFlags rejects the flags that do not apply to an EDL flash.
func checkDragonwingFlags(board dragonwingBoard, rootfsOnly bool, drive string, noBmap, overwriteInternal bool,
	storageOverride string) error {
	name := humanReadableDeviceType(board.deviceType)
	if rootfsOnly {
		return fmt.Errorf("--rootfs-only is not available for the %s", name)
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
		return fmt.Errorf("%s %s apply to the %s: it is flashed over EDL, and the partition layout comes from the bundle rather than a host drive",
			strings.Join(drivish, ", "), verb, name)
	}
	if storageOverride != "" {
		return fmt.Errorf("--storage does not apply to the %s: it flashes its onboard UFS", name)
	}
	return nil
}

// dragonwingTargetLabel names the physical device (serial, CID, USB address),
// not the board model — the chip id confirms that only when the board answers.
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
