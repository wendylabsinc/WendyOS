//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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

// liteBuildPickerItem is the install picker entry of runLiteBuild.
func liteBuildPickerItem() tui.PickerItem {
	return tui.PickerItem{
		Name:        "ESP32 platform via new configurator",
		Description: "Build Wendy Lite from source with the configurator",
		Section:     "Wendy Lite",
		// Before the "1_lite_<target>" entries: first in its tab.
		SortKey: "1_lite_0_build",
		Value:   liteBuildValue,
	}
}

// Seams for tests.
var (
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
//  2. check git, then reuse or clone the build folder of the commit of the
//     wendy-lite tag or branch (see liteBuildRef and openLiteBuildFolder)
//  3. ask the configurator questions of the folder's master configuration;
//     the answers of the last run are the defaults
//  4. ask for Wi-Fi, the device name and Cloud enrollment: the content of
//     the wendy_conf partition
//  5. install the ESP-IDF version that the answers need
//  6. run tools/configure.py with the answers when the folder is not
//     configured for them (liteBuildConfigureReason), then idf.py build
//  7. idf.py merge-bin, and write wendy_conf into the merged image
//  8. flash it as the catalog firmware is flashed, then enroll
//
// The build folder stays in the cache for the next build of the same
// commit, which is incremental when the answers are the same.
func runLiteBuild(ctx context.Context, nightly bool, version string, wifi wifiCLIOptions, deviceName string, preOpts preEnrollOptions) error {
	if jsonOutput || !isInteractiveTerminal() {
		return usageErrorf("the Wendy Lite build asks questions: run it in an interactive terminal, without --json")
	}

	if err := ensureLiteBuildEIM(ctx); err != nil {
		return err
	}

	if err := litebuild.CheckGit(); err != nil {
		return err
	}
	ref := liteBuildRef(nightly, version)
	dir, fullCommit, release, err := openLiteBuildFolder(ctx, espidftoolchain.WendyLiteGitURL, ref)
	if err != nil {
		return err
	}
	defer release()
	commit := litebuild.ShortVersion(fullCommit)

	// Questions, from the master configuration of the folder: the code that
	// is built.
	model, err := litebuild.LoadFile(litebuild.MasterConfPath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("wendy-lite %s (%s) has no master_config.yaml", ref, commit)
	}
	if err != nil {
		return err
	}
	configurator, err := litebuild.NewConfigurator(model, nil)
	if err != nil {
		return err
	}
	answersPath, answersErr := liteBuildAnswersPath()
	if answersErr == nil {
		configurator.Previous = litebuild.LoadAnswers(model, answersPath)
	}
	fmt.Println()
	result, err := configurator.Run(&litebuild.TUIPrompter{In: os.Stdin, Out: os.Stderr})
	if errors.Is(err, tui.ErrCancelled) {
		return ErrUserCancelled
	}
	if err != nil {
		return err
	}
	if answersErr == nil {
		answersErr = litebuild.SaveAnswers(model, answersPath, configurator.Previous, result.Inputs)
	}
	if answersErr != nil {
		fmt.Fprintln(os.Stderr, tui.WarningMessage(fmt.Sprintf("The answers are not saved for the next run: %v", answersErr)))
	}
	build, err := liteBuildSteps(model, result)
	if err != nil {
		return err
	}
	// Before the long build: the flasher must know the chip.
	chip, err := chipModelForTarget(build.chip)
	if err != nil {
		return err
	}
	// Not used by the build: written to compare with configure.py's
	// sdkconfig.defaults.wendy.
	sdkconfigText := model.SdkconfigText(model.Header(result.Inputs), result.Inputs, result.Solution)
	if err := os.WriteFile(filepath.Join(dir, "sdkconfig.wendy.go"), []byte(sdkconfigText), 0o644); err != nil {
		return err
	}

	// Device configuration: the wendy_conf partition. It holds the Wi-Fi
	// password, so it stays in memory, out of the build folder.
	prov, err := resolveLiteProvisioning(wifi, deviceName, preOpts)
	if err != nil {
		return err
	}
	payload, err := wendyConfPayload(prov.conf)
	if err != nil {
		return fmt.Errorf("serializing device config: %w", err)
	}

	fmt.Println()
	if err := espidftoolchain.EnsureIDF(ctx, build.eimVersion); err != nil {
		return err
	}

	fmt.Printf("\nBuild folder: %s\n", dir)
	sourceVersion, err := litebuild.SourceVersion(ctx, dir)
	if err != nil {
		return err
	}
	reason, err := liteBuildConfigureReason(dir, model.BuildProfile(result.Inputs), sourceVersion, build.eimVersion)
	if err != nil {
		return err
	}
	if reason == "" {
		fmt.Println("The build folder is configured for these answers: tools/configure.py is skipped.")
	} else {
		fmt.Printf("Configuring the build: %s.\n", reason)
		if err := resetLiteBuildFolder(ctx, dir); err != nil {
			return err
		}
		configureArgs := append([]string{"tools/configure.py"}, build.configure...)
		if err := runLiteBuildCommand(ctx, dir, build.eimVersion, "python", configureArgs...); err != nil {
			return fmt.Errorf("tools/configure.py: %w", err)
		}
	}
	if err := runLiteBuildCommand(ctx, dir, build.eimVersion, "idf.py", build.idfArgs("build")...); err != nil {
		return fmt.Errorf("idf.py build: %w", err)
	}
	if err := runLiteBuildCommand(ctx, dir, build.eimVersion, "idf.py", build.idfArgs("merge-bin")...); err != nil {
		return fmt.Errorf("idf.py merge-bin: %w", err)
	}

	img, err := LoadEspFlashImage(filepath.Join(dir, "build", "merged-binary.bin"))
	if err != nil {
		return fmt.Errorf("loading the merged image: %w", err)
	}
	if err := img.SetPartition("wendy_conf", payload); err != nil {
		return fmt.Errorf("writing config to firmware image: %w", err)
	}
	// The image is in memory: another build can use the cache during the
	// flash.
	release()
	fmt.Printf("\nBuilt Wendy Lite %s (%s) for the %s.\n", ref, commit, chipModelName(chip))

	port, autoDiscover, err := detectLiteInstallPort(discovery.SerialPortInfo{}, true)
	if err != nil {
		return err
	}
	if err := checkLiteInstallPort(port, prov); err != nil {
		return err
	}
	// No version: the device reports the version it was built with.
	return flashLiteImage(ctx, img, chip, port, autoDiscover, prov, "")
}

// ensureLiteBuildEIM checks that EIM is installed, and offers to install it
// when it is not.
func ensureLiteBuildEIM(ctx context.Context) error {
	err := espidftoolchain.CheckEIM(ctx)
	if !errors.Is(err, espidftoolchain.ErrEIMNotInstalled) {
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

// liteBuildRef returns the wendy-lite tag or branch to build: "nightly"
// with --nightly, the --version value, else litebuild.DefaultRef.
func liteBuildRef(nightly bool, version string) string {
	switch {
	case nightly:
		return "nightly"
	case version != "":
		return version
	}
	return litebuild.DefaultRef
}

// openLiteBuildFolder returns the build folder of the commit of ref, the
// tag or branch of the wendy-lite repository at url, and the commit. It locks
// the build cache first: release unlocks it, and calling it more than once is
// harmless. Then it removes the old build folders (litebuild.PruneBuildFolders),
// and reuses or clones the folder of the commit (litebuild.PrepareSources).
func openLiteBuildFolder(ctx context.Context, url, ref string) (dir, commit string, release func(), err error) {
	root, err := liteBuildRoot()
	if err != nil {
		return "", "", nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", nil, err
	}
	// One lock for the cache: the cleanup removes folders that another
	// build could use.
	unlock, err := flock.Acquire(filepath.Join(root, ".lock"), 0)
	if errors.Is(err, flock.ErrTimeout) {
		return "", "", nil, fmt.Errorf("another Wendy Lite build is running (it uses %s): wait until it ends", root)
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("locking the Wendy Lite build cache %s: %w", root, err)
	}
	defer func() {
		if err != nil {
			unlock()
		}
	}()

	fmt.Printf("Looking up wendy-lite %s at %s...\n", ref, url)
	commit, tag, err := litebuild.ResolveRef(ctx, url, ref)
	if err != nil {
		return "", "", nil, err
	}
	logf := func(format string, args ...any) { fmt.Printf(format+"\n", args...) }
	warnf := func(format string, args ...any) {
		fmt.Fprintln(os.Stderr, tui.WarningMessage(fmt.Sprintf(format, args...)))
	}
	litebuild.PruneBuildFolders(root, commit, logf, warnf)
	dir, commit, err = litebuild.PrepareSources(ctx, root, url, ref, commit, tag, logf)
	if err != nil {
		return "", "", nil, err
	}
	return dir, commit, unlock, nil
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
// version (an eim name), with the output in the terminal.
func runLiteBuildCommand(ctx context.Context, dir, version, name string, args ...string) error {
	cmd := espidftoolchain.EIMCommandContext(ctx, version, name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	fmt.Printf("\n%s\n", tui.Dim(fmt.Sprintf("Running in ESP-IDF %s: %s %s", version, name, strings.Join(args, " "))))
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
		return liteBuild{}, fmt.Errorf("the configurator gave no %s", litebuild.Chip)
	}
	idfVersion, ok := values[litebuild.IDFVersion].(string)
	if !ok {
		return liteBuild{}, fmt.Errorf("the configurator gave no %s", litebuild.IDFVersion)
	}
	preview, ok := values[litebuild.IDFPreview].(bool)
	if !ok {
		return liteBuild{}, fmt.Errorf("the configurator gave no %s", litebuild.IDFPreview)
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
