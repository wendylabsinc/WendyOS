//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/internal/cli/espidftoolchain"
	"github.com/wendylabsinc/wendy/go/internal/cli/litebuild"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
)

// liteBuildValue is the install picker value of the entry that builds Wendy
// Lite from source with the configurator, then flashes it. The wlite_ prefix
// puts it in the Microcontrollers tab.
const liteBuildValue = "wlite_build"

// liteBuildDeviceType is the --device-type of the same entry.
const liteBuildDeviceType = "esp32"

// liteBuildS31Value is the install picker value of a detected ESP32-S31 in
// ROM download mode: the Wendy Lite build with the S31 chip given.
const liteBuildS31Value = "wlite_build_esp32s31"

// liteCatalogInstallEnv brings back the installs of the published Wendy
// Lite firmware: the ESP32 targets of the install picker (then a board
// model), a detected ESP32-S31 leading to them, and the reinstall offer of
// wendy run. Without it, all of them build Wendy Lite (runLiteBuild).
// Deliberately undocumented.
const liteCatalogInstallEnv = "WENDY_LITE_CATALOG_INSTALL"

// liteCatalogInstallEnabled reports whether liteCatalogInstallEnv is true.
func liteCatalogInstallEnabled() bool {
	enabled, err := strconv.ParseBool(os.Getenv(liteCatalogInstallEnv))
	return err == nil && enabled
}

// litePickerItems returns the Microcontrollers entries of the install
// picker: the published-firmware targets when liteCatalogInstallEnabled,
// with their boards for the picker's device map, then the Wendy Lite build.
func litePickerItems(nightly bool) (items []tui.PickerItem, boards []WendyLiteBoard) {
	if liteCatalogInstallEnabled() {
		var targets []WendyLiteTarget
		boards, targets = fetchLiteCatalog(nightly)
		for _, t := range targets {
			items = append(items, tui.PickerItem{
				Name:    t.DisplayName,
				Section: "Wendy Lite",
				SortKey: "1_lite_" + strings.ToLower(t.DisplayName),
				Value:   "wlite_target_" + t.Name,
			})
		}
	}
	return append(items, liteBuildPickerItem()), boards
}

// liteBuildOptionsFor returns the build options of the install picker
// selection, and false when the selection is not a Wendy Lite build. A
// detected ESP32-S31 gives its chip.
func liteBuildOptionsFor(selected string, o liteBuildOptions) (liteBuildOptions, bool) {
	switch selected {
	case liteBuildValue:
		return o, true
	case liteBuildS31Value:
		o.required = map[string]string{litebuild.Chip: "esp32s31"}
		return o, true
	}
	return o, false
}

// liteBuildOptions are the inputs of runLiteBuild.
type liteBuildOptions struct {
	nightly bool
	version string // --version: a Wendy Lite version, branch or commit
	// attrs are the --build-attr flags: a bad name or value is an error.
	attrs map[string]string
	// required are values that the caller knows: the chip of a detected
	// board, the app type that wendy run needs. A --build-attr of the same
	// name wins. A value that the master configuration does not accept is
	// skipped: the chip check before flashing still protects the board.
	required   map[string]string
	wifi       wifiCLIOptions
	deviceName string
	preOpts    preEnrollOptions
	// port is the board to flash; empty, the ESP32 ports are scanned.
	port discovery.SerialPortInfo
	// versionHint replaces "Specify a Wendy Lite version with --version" in
	// the error of a Wendy Lite version that cannot be built.
	versionHint string

	// simulator is the profile of the simulator that the firmware is built
	// for (see createLiteSimulator). attrs are then the profile's values,
	// nothing is asked, and the answers of the last install are neither used
	// nor replaced.
	simulator string
	// out receives the build output; nil is stdout.
	out io.Writer
	// beforeBuild runs after the questions, before ESP-IDF is installed and
	// the build starts: the install asks for the device configuration there.
	beforeBuild func() error
}

// output returns the writer of the build output.
func (o liteBuildOptions) output() io.Writer {
	if o.out == nil {
		return os.Stdout
	}
	return o.out
}

// parseBuildAttrs reads the --build-attr flags: NAME=VALUE, each name once.
// The names and values are checked against the master configuration later,
// when it is cloned.
func parseBuildAttrs(items []string) (map[string]string, error) {
	attrs := map[string]string{}
	for _, item := range items {
		name, value, found := strings.Cut(item, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		switch {
		case !found || name == "":
			return nil, usageErrorf("--build-attr %q: expected NAME=VALUE", item)
		case value == "":
			return nil, usageErrorf("--build-attr %s: give a value", name)
		}
		if _, ok := attrs[name]; ok {
			return nil, usageErrorf("--build-attr %s is given twice", name)
		}
		attrs[name] = value
	}
	return attrs, nil
}

// liteBuildIgnoredFlags are the install flags that the Wendy Lite build
// does not use: it builds the firmware and flashes it over USB.
var liteBuildIgnoredFlags = []string{"pr", "driver", "binary", "drive", "storage", "rootfs-only"}

// checkLiteBuildFlags refuses, before anything is fetched, --build-attr
// for an install that is not the Wendy Lite build, and the flags that
// --device-type esp32 would ignore.
func checkLiteBuildFlags(cmd *cobra.Command, deviceType string, positional, buildAttrs bool) error {
	if deviceType != liteBuildDeviceType {
		if buildAttrs && (deviceType != "" || positional) {
			return usageErrorf("--build-attr applies only to --device-type %s", liteBuildDeviceType)
		}
		return nil
	}
	// Without a terminal, root turns JSON on by itself: only an explicit
	// --json is refused.
	if cmd.Root().PersistentFlags().Changed("json") && jsonOutput {
		return usageErrorf("--device-type %s does not support --json", liteBuildDeviceType)
	}
	var ignored []string
	for _, name := range liteBuildIgnoredFlags {
		if cmd.Flags().Changed(name) {
			ignored = append(ignored, "--"+name)
		}
	}
	if len(ignored) > 0 {
		return usageErrorf("--device-type %s builds Wendy Lite and flashes it over USB: remove %s", liteBuildDeviceType, strings.Join(ignored, ", "))
	}
	return nil
}

// liteBuildGiven returns the configurator values of the --build-attr flags
// (attrs), checked against the master configuration m, and the required
// values that m accepts and attrs does not name.
func liteBuildGiven(m *litebuild.Model, attrs, required map[string]string) (map[string]any, error) {
	given := map[string]any{}
	for name, text := range required {
		if _, ok := attrs[name]; ok {
			continue
		}
		if value, err := m.ParseValue(name, text); err == nil && value != nil {
			given[name] = value
		}
	}
	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		value, err := m.ParseValue(name, attrs[name])
		if err != nil {
			if m.Attributes[name] == nil {
				names := make([]string, len(m.Order))
				for i, a := range m.Order {
					names[i] = a.Name
				}
				return nil, usageErrorf("--build-attr %s=%s: %v (attributes: %s)", name, attrs[name], err, strings.Join(names, ", "))
			}
			return nil, usageErrorf("--build-attr %s=%s: %v", name, attrs[name], err)
		}
		given[name] = value
	}
	return given, nil
}

// noTerminalPrompter answers no question: without a terminal, every
// question needs a --build-attr flag.
type noTerminalPrompter struct {
	litebuild.TUIPrompter // for Info
}

func (p *noTerminalPrompter) Select(q litebuild.Question) (int, error) {
	return 0, noTerminalQuestion(q)
}

func (p *noTerminalPrompter) Confirm(q litebuild.Question) (bool, error) {
	return false, noTerminalQuestion(q)
}

func (p *noTerminalPrompter) Text(q litebuild.Question, _ func(string) error) (string, error) {
	return "", noTerminalQuestion(q)
}

// noTerminalQuestion is the error of a question without a terminal: it
// names the flag that answers it, with the allowed values.
func noTerminalQuestion(q litebuild.Question) error {
	if len(q.Options) == 0 {
		return usageErrorf("no terminal to ask %q: pass --build-attr %s=<value>", q.Attr.Title(), q.Attr.Name)
	}
	keys := make([]string, len(q.Options))
	for i, v := range q.Options {
		keys[i] = q.Attr.Key(v)
	}
	return usageErrorf("no terminal to ask %q: pass --build-attr %s=<value> (allowed: %s)", q.Attr.Title(), q.Attr.Name, strings.Join(keys, ", "))
}

// liteBuildPickerItem is the install picker entry of runLiteBuild.
func liteBuildPickerItem() tui.PickerItem {
	return tui.PickerItem{
		Name:        "ESP32 platform",
		Description: "Build Wendy Lite for your board and flash it",
		Section:     "Wendy Lite",
		// Before the "1_lite_<target>" entries: first in its tab.
		SortKey: "1_lite_0_build",
		Value:   liteBuildValue,
	}
}

// Seams for tests.
var (
	runLiteBuildFn = runLiteBuild

	// buildLiteFirmwareFn builds the firmware of a Wendy Lite simulator.
	buildLiteFirmwareFn = buildLiteFirmware

	// fetchLiteCatalog returns the boards with published firmware and their
	// targets: a catalog entry with no build yet would otherwise reach the
	// picker and fail immediately after selection, and a target with no
	// board would lead to an empty second picker.
	fetchLiteCatalog = func(nightly bool) ([]WendyLiteBoard, []WendyLiteTarget) {
		boards, err := WendyLiteBoardsWithFirmware(nightly)
		if err != nil {
			log.Printf("WARNING: could not fetch Wendy Lite board catalog: %v", err)
		}
		targets, err := WendyLiteTargets()
		if err != nil {
			log.Printf("WARNING: could not fetch Wendy Lite target catalog: %v", err)
		}
		boardTargets := make(map[string]bool, len(boards))
		for _, b := range boards {
			boardTargets[b.Target] = true
		}
		var withBoards []WendyLiteTarget
		for _, t := range targets {
			if boardTargets[t.Name] {
				withBoards = append(withBoards, t)
			}
		}
		return boards, withBoards
	}

	// liteBuildRoot returns the cache folder of the Wendy Lite build: the
	// build folders (see litebuild.PrepareSources), the answers and the
	// lock.
	liteBuildRoot = func() (string, error) {
		dir, err := config.CacheDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "wendy-lite-build"), nil
	}

	// liteBuildAnswersPath returns the file that keeps the configurator
	// answers between runs.
	liteBuildAnswersPath = func() (string, error) {
		root, err := liteBuildRoot()
		if err != nil {
			return "", err
		}
		return filepath.Join(root, "answers.json"), nil
	}
)

// runLiteBuild builds Wendy Lite from source and flashes it:
//
//  1. check EIM, the ESP-IDF Installation Manager
//  2. check git, then resolve the Wendy Lite version (the latest release, the
//     nightly tag, or --version), check that it has a master configuration,
//     and reuse or clone the build folder of its commit (see
//     openLiteBuildFolder)
//  3. ask the configurator questions of the folder's master configuration,
//     except the ones that attrs (the --build-attr flags) answer; the
//     answers of the last run are the defaults. Without a terminal, every
//     question needs a --build-attr flag
//  4. ask for Wi-Fi, the device name and Cloud enrollment: the content of
//     the wendy_conf partition
//  5. install the ESP-IDF version that the answers need
//  6. run tools/configure.py with the answers when the folder is not
//     configured for them (liteBuildConfigureReason), then idf.py build
//  7. idf.py merge-bin, and write wendy_conf into the merged image
//  8. flash it as the catalog firmware is flashed, then enroll
//
// Steps 1 to 7 are buildLiteFirmware, with step 4 as its beforeBuild. The
// build folder stays in the cache for the next build of the same commit,
// which is incremental when the answers are the same.
func runLiteBuild(ctx context.Context, o liteBuildOptions) error {
	interactive := isInteractiveTerminal()
	// In a terminal, JSON is on only with an explicit --json (see
	// startInstallSession). Without one, root turns it on by itself, and
	// checkLiteBuildFlags refuses an explicit --json.
	if interactive && jsonOutput {
		return usageErrorf("the Wendy Lite build does not support --json")
	}

	// Device configuration: the wendy_conf partition. It holds the Wi-Fi
	// password, so it stays in memory, out of the build folder.
	var prov *liteProvisioning
	var payload []byte
	o.beforeBuild = func() error {
		var err error
		prov, err = resolveLiteProvisioning(ctx, o.wifi, o.deviceName, o.preOpts)
		if err != nil {
			return err
		}
		payload, err = wendyConfPayload(prov.conf)
		if err != nil {
			return fmt.Errorf("serializing device config: %w", err)
		}
		return nil
	}
	fw, err := buildLiteFirmware(ctx, o)
	if err != nil {
		return err
	}
	if err := fw.img.SetPartition("wendy_conf", payload); err != nil {
		return fmt.Errorf("writing config to firmware image: %w", err)
	}
	fmt.Printf("\nBuilt Wendy Lite %s for the %s.\n", fw.src, chipModelName(fw.chip))

	port, autoDiscover, err := detectLiteInstallPort(o.port, interactive)
	if err != nil {
		return err
	}
	if err := checkLiteInstallPort(port, prov); err != nil {
		return err
	}
	// No version: the device reports the version it was built with.
	return flashLiteImage(ctx, fw.img, fw.chip, port, autoDiscover, prov, "")
}

// liteFirmware is a Wendy Lite build: the merged flash image, without the
// wendy_conf partition.
type liteFirmware struct {
	img  *EspFlashImage
	chip chipModel
	src  litebuild.Source
}

// buildLiteFirmware builds Wendy Lite from source: steps 1 to 7 of
// runLiteBuild, with o.beforeBuild as step 4, and without writing
// wendy_conf. The image is in memory, and the build cache is unlocked when
// it returns.
func buildLiteFirmware(ctx context.Context, o liteBuildOptions) (*liteFirmware, error) {
	out := o.output()
	interactive := isInteractiveTerminal()

	if err := ensureLiteBuildEIM(ctx, interactive || o.simulator == ""); err != nil {
		return nil, err
	}

	if err := litebuild.CheckGit(); err != nil {
		return nil, err
	}
	dir, src, release, err := openLiteBuildFolder(ctx, out, espidftoolchain.WendyLiteGitURL, o.nightly, o.version)
	if err != nil {
		return nil, o.withVersionHint(err)
	}
	defer release()

	// Questions, from the master configuration of the folder: the code that
	// is built.
	model, err := litebuild.LoadFile(litebuild.MasterConfPath(dir))
	var formatErr *litebuild.FormatVersionError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, o.withVersionHint(liteBuildIncompatible(src))
	case errors.As(err, &formatErr):
		return nil, o.withVersionHint(&liteBuildIncompatibleError{src: src, format: formatErr})
	case err != nil:
		return nil, err
	}
	configurator, err := litebuild.NewConfigurator(model, nil)
	if err != nil {
		return nil, err
	}
	if o.simulator != "" {
		configurator.Given, err = liteSimulatorGiven(model, src, o.simulator, o.attrs)
	} else {
		configurator.Given, err = liteBuildGiven(model, o.attrs, o.required)
	}
	if err != nil {
		return nil, err
	}
	answersPath, answersErr := liteBuildAnswersPath()
	// Without a terminal, the answers of the last run are not used: the
	// result depends only on the command line. A simulator build asks
	// nothing, and keeps the answers of the install.
	saveAnswers := o.simulator == ""
	if answersErr == nil && interactive && saveAnswers {
		configurator.Previous = litebuild.LoadAnswers(model, answersPath)
	}
	var prompter litebuild.Prompter = &litebuild.TUIPrompter{In: os.Stdin, Out: os.Stderr}
	switch {
	case o.simulator != "":
		prompter = &liteSimulatorPrompter{TUIPrompter: litebuild.TUIPrompter{Out: os.Stderr}, src: src, profile: o.simulator}
	case !interactive:
		prompter = &noTerminalPrompter{litebuild.TUIPrompter{Out: os.Stderr}}
	}
	fmt.Fprintln(out)
	result, err := configurator.Run(prompter)
	if errors.Is(err, tui.ErrCancelled) {
		return nil, ErrUserCancelled
	}
	var givenErr *litebuild.GivenError
	if errors.As(err, &givenErr) {
		if o.simulator != "" {
			return nil, liteSimulatorIncompatible(src, o.simulator, err)
		}
		if _, ok := o.attrs[givenErr.Name]; ok {
			return nil, usageErrorf("--build-attr %v", err)
		}
		return nil, fmt.Errorf("Wendy Lite cannot be built for this board: %w", err)
	}
	if err != nil {
		return nil, err
	}
	if saveAnswers {
		if answersErr == nil {
			answersErr = litebuild.SaveAnswers(model, answersPath, configurator.Previous, result.Inputs)
		}
		if answersErr != nil {
			fmt.Fprintln(os.Stderr, tui.WarningMessage(fmt.Sprintf("The answers are not saved for the next run: %v", answersErr)))
		}
	}
	build, err := liteBuildSteps(model, result)
	if err != nil {
		return nil, err
	}
	// Before the long build: the flasher must know the chip.
	chip, err := chipModelForTarget(build.chip)
	if err != nil {
		return nil, err
	}
	// Not used by the build: written to compare with configure.py's
	// sdkconfig.defaults.wendy.
	sdkconfigText := model.SdkconfigText(model.Header(result.Inputs), result.Inputs, result.Solution)
	if err := os.WriteFile(filepath.Join(dir, "sdkconfig.wendy.go"), []byte(sdkconfigText), 0o644); err != nil {
		return nil, err
	}

	if o.beforeBuild != nil {
		if err := o.beforeBuild(); err != nil {
			return nil, err
		}
	}

	fmt.Fprintln(out)
	if err := espidftoolchain.EnsureIDFTo(ctx, build.eimVersion, out); err != nil {
		return nil, err
	}

	fmt.Fprintf(out, "\nBuild folder: %s\n", dir)
	sourceVersion, err := litebuild.SourceVersion(ctx, dir)
	if err != nil {
		return nil, err
	}
	reason, err := liteBuildConfigureReason(dir, model.BuildProfile(result.Inputs), sourceVersion, build.eimVersion)
	if err != nil {
		return nil, err
	}
	if reason == "" {
		fmt.Fprintln(out, "The build folder is configured for these answers: tools/configure.py is skipped.")
	} else {
		fmt.Fprintf(out, "Configuring the build: %s.\n", reason)
		if err := resetLiteBuildFolder(ctx, dir); err != nil {
			return nil, err
		}
		configureArgs := append([]string{"tools/configure.py"}, build.configure...)
		if err := runLiteBuildCommand(ctx, out, dir, build.eimVersion, "python", configureArgs...); err != nil {
			return nil, fmt.Errorf("tools/configure.py: %w", err)
		}
	}
	if err := runLiteBuildCommand(ctx, out, dir, build.eimVersion, "idf.py", build.idfArgs("build")...); err != nil {
		return nil, fmt.Errorf("idf.py build: %w", err)
	}
	if err := runLiteBuildCommand(ctx, out, dir, build.eimVersion, "idf.py", build.idfArgs("merge-bin")...); err != nil {
		return nil, fmt.Errorf("idf.py merge-bin: %w", err)
	}

	img, err := LoadEspFlashImage(filepath.Join(dir, "build", "merged-binary.bin"))
	if err != nil {
		return nil, fmt.Errorf("loading the merged image: %w", err)
	}
	// The image is in memory: another build can use the cache now, during
	// the flash for example.
	release()
	return &liteFirmware{img: img, chip: chip, src: src}, nil
}

// ensureLiteBuildEIM checks that EIM is installed, and offers to install it
// when it is not and offer is true.
func ensureLiteBuildEIM(ctx context.Context, offer bool) error {
	err := espidftoolchain.CheckEIM(ctx)
	if !errors.Is(err, espidftoolchain.ErrEIMNotInstalled) || !offer {
		return err
	}
	installed, installErr := installMissingEIM(ctx, false)
	if installErr != nil {
		return installErr
	}
	if !installed {
		return err
	}
	return nil
}

// openLiteBuildFolder returns the build folder of the Wendy Lite version to
// build, and that version: the nightly tag with nightly, else version, else
// the latest release (see litebuild.ResolveSource) of the wendy-lite
// repository at url. Its progress goes to out. It locks the build cache
// first: release unlocks it, and calling it more than once is harmless. It checks that the version has a master
// configuration (checkLiteBuildSource): a version without it changes
// nothing in the cache. Then it removes the old build folders
// (litebuild.PruneBuildFolders), and reuses or clones the folder of the
// commit (litebuild.PrepareSources).
func openLiteBuildFolder(ctx context.Context, out io.Writer, url string, nightly bool, version string) (dir string, src litebuild.Source, release func(), err error) {
	root, err := liteBuildRoot()
	if err != nil {
		return "", litebuild.Source{}, nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", litebuild.Source{}, nil, err
	}
	// One lock for the cache: the cleanup removes folders that another
	// build could use.
	unlock, err := flock.Acquire(filepath.Join(root, ".lock"), 0)
	if errors.Is(err, flock.ErrTimeout) {
		return "", litebuild.Source{}, nil, fmt.Errorf("another Wendy Lite build is running (it uses %s): wait until it ends", root)
	}
	if err != nil {
		return "", litebuild.Source{}, nil, fmt.Errorf("locking the Wendy Lite build cache %s: %w", root, err)
	}
	defer func() {
		if err != nil {
			unlock()
		}
	}()

	fmt.Fprintf(out, "Looking up %s at %s...\n", liteBuildRequest(nightly, version), url)
	src, err = litebuild.ResolveSource(ctx, url, nightly, version)
	if err != nil {
		return "", litebuild.Source{}, nil, err
	}
	if err := checkLiteBuildSource(ctx, root, url, src); err != nil {
		return "", litebuild.Source{}, nil, err
	}
	logf := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
	warnf := func(format string, args ...any) {
		fmt.Fprintln(os.Stderr, tui.WarningMessage(fmt.Sprintf(format, args...)))
	}
	litebuild.PruneBuildFolders(root, src.Commit, logf, warnf)
	dir, err = litebuild.PrepareSources(ctx, root, url, src, logf)
	if err != nil {
		return "", litebuild.Source{}, nil, err
	}
	return dir, src, unlock, nil
}

// liteBuildRequest names the Wendy Lite version that nightly and version
// ask for, before litebuild.ResolveSource resolves it.
func liteBuildRequest(nightly bool, version string) string {
	switch {
	case nightly:
		return "the wendy-lite " + litebuild.NightlyTag + " tag"
	case version != "":
		return "wendy-lite " + version
	}
	return "the latest Wendy Lite release"
}

// checkLiteBuildSource checks that src, a version of the wendy-lite
// repository at url, has a master configuration in a format that this CLI
// reads: the versions without it are older than the configurator, and a
// newer format needs a newer CLI. It reads the file in the build folder of
// src in root when the folder has it, else the file of src at url.
func checkLiteBuildSource(ctx context.Context, root, url string, src litebuild.Source) error {
	folder := filepath.Join(root, litebuild.BuildFolderName(src.Commit))
	data, err := os.ReadFile(litebuild.MasterConfPath(folder))
	if err != nil {
		var found bool
		data, found, err = litebuild.LookupMasterConf(ctx, url, src.Commit)
		if err != nil {
			return err
		}
		if !found {
			return liteBuildIncompatible(src)
		}
	}
	var formatErr *litebuild.FormatVersionError
	if errors.As(litebuild.CheckFormatVersion(data), &formatErr) {
		return &liteBuildIncompatibleError{src: src, format: formatErr}
	}
	return nil
}

// liteBuildIncompatibleError is the error of a Wendy Lite version that this
// CLI cannot build: it has no master configuration, or one in a format
// that this CLI does not read (format). hint says what to do instead; the
// default names --version.
type liteBuildIncompatibleError struct {
	src    litebuild.Source
	format *litebuild.FormatVersionError
	hint   string
}

func (e *liteBuildIncompatibleError) Error() string {
	hint := e.hint
	if hint == "" {
		hint = "Specify a Wendy Lite version with --version"
	}
	if e.format != nil && e.format.Newer {
		// "Update the CLI or specify a Wendy Lite version with --version"
		return fmt.Sprintf("Wendy Lite %s needs a newer version of the CLI. Update the CLI or %s",
			e.src, strings.ToLower(hint[:1])+hint[1:])
	}
	return fmt.Sprintf("Wendy Lite %s is not compatible with this version of the CLI. %s", e.src, hint)
}

func (e *liteBuildIncompatibleError) Unwrap() error {
	if e.format == nil {
		return nil
	}
	return e.format
}

// liteBuildIncompatible is the error of a Wendy Lite version without master
// configuration.
func liteBuildIncompatible(src litebuild.Source) error {
	return &liteBuildIncompatibleError{src: src}
}

// withVersionHint returns err, or, when o.versionHint is set and err is (or
// wraps) a liteBuildIncompatible error, that error with o.versionHint.
func (o liteBuildOptions) withVersionHint(err error) error {
	var incompatible *liteBuildIncompatibleError
	if o.versionHint != "" && errors.As(err, &incompatible) {
		return &liteBuildIncompatibleError{src: incompatible.src, format: incompatible.format, hint: o.versionHint}
	}
	return err
}

// liteBuildConfigureReason tells why the wendy-lite checkout in dir needs
// tools/configure.py for the build profile and the version (see
// litebuild.ConfigureReason), or for ESP-IDF eimVersion: the CMake
// configuration is missing, or was made with another ESP-IDF version. It
// returns "" when the checkout is configured, and idf.py build is then
// incremental.
func liteBuildConfigureReason(dir, profile, version, eimVersion string) (string, error) {
	reason, err := litebuild.ConfigureReason(dir, profile, version)
	if err != nil || reason != "" {
		return reason, err
	}
	if _, err := os.Stat(filepath.Join(dir, "build", "CMakeCache.txt")); err != nil {
		return "the build folder has no CMake configuration", nil
	}
	if configured := espidftoolchain.BuildIDFVersion(dir); configured != "" && !espidftoolchain.SameIDFVersion(configured, eimVersion) {
		return fmt.Sprintf("the build folder was configured with ESP-IDF %s, and this build uses ESP-IDF %s", configured, eimVersion), nil
	}
	return "", nil
}

// resetLiteBuildFolder prepares the wendy-lite checkout in dir for
// tools/configure.py. It deletes the ESP-IDF build folder, as the fullclean
// of idf.py set-target does: deleting it here also works when another
// ESP-IDF version made it, or when fullclean refuses it (no CMakeCache.txt).
// It restores the tracked files, which a build changes (the component
// manager rewrites dependencies.lock for its target): configure.py would
// otherwise add -dirty to the version.
func resetLiteBuildFolder(ctx context.Context, dir string) error {
	if err := os.RemoveAll(filepath.Join(dir, "build")); err != nil {
		return fmt.Errorf("removing the ESP-IDF build folder: %w", err)
	}
	if _, err := litebuild.Git(ctx, dir, "reset", "--quiet", "--hard"); err != nil {
		return fmt.Errorf("restoring the wendy-lite sources: %w", err)
	}
	return nil
}

// runLiteBuildCommand runs name in dir, in the environment of the ESP-IDF
// version (an eim name), with its standard output in out and its errors in
// the terminal.
func runLiteBuildCommand(ctx context.Context, out io.Writer, dir, version, name string, args ...string) error {
	cmd := espidftoolchain.EIMCommandContext(ctx, version, name, args...)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	fmt.Fprintf(out, "\n%s\n", tui.Dim(fmt.Sprintf("Running in ESP-IDF %s: %s %s", version, name, strings.Join(args, " "))))
	return cmd.Run()
}

// liteBuild is what the build needs from the configurator result.
type liteBuild struct {
	chip       string   // the IDF target, for example "esp32s3"
	eimVersion string   // the eim name of the ESP-IDF version, for example "v6.0.3"
	preview    bool     // idf.py --preview
	configure  []string // the NAME=VALUE arguments of configure.py
}

// liteBuildSteps returns what the build needs from the configurator result.
// configure.py gets the answers and the only allowed values: it then gives
// the same solution.
func liteBuildSteps(m *litebuild.Model, res *litebuild.Result) (liteBuild, error) {
	values := res.Solution.Values
	chip, ok := values[litebuild.Chip].(string)
	if !ok {
		return liteBuild{}, fmt.Errorf("the build configuration has no %s", litebuild.Chip)
	}
	idfVersion, ok := values[litebuild.IDFVersion].(string)
	if !ok {
		return liteBuild{}, fmt.Errorf("the build configuration has no %s", litebuild.IDFVersion)
	}
	preview, ok := values[litebuild.IDFPreview].(bool)
	if !ok {
		return liteBuild{}, fmt.Errorf("the build configuration has no %s", litebuild.IDFPreview)
	}
	return liteBuild{
		chip:       chip,
		eimVersion: espidftoolchain.EIMVersion(idfVersion),
		preview:    preview,
		configure:  m.Assignments(res.Inputs),
	}, nil
}

// idfArgs returns the idf.py arguments, with --preview first when the chip
// is an ESP-IDF preview target.
func (b liteBuild) idfArgs(args ...string) []string {
	if b.preview {
		return append([]string{"--preview"}, args...)
	}
	return args
}
