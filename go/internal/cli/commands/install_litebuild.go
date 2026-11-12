//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/espidftoolchain"
	"github.com/wendylabsinc/wendy/go/internal/cli/litebuild"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
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
	// liteBuildAnswersPath returns the file that keeps the configurator
	// answers between runs.
	liteBuildAnswersPath = func() (string, error) {
		dir, err := config.CacheDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "wendy-lite-build", "answers.json"), nil
	}
	liteBuildLookPath = exec.LookPath
)

// runLiteBuild builds Wendy Lite from source and flashes it:
//
//  1. check EIM, the ESP-IDF Installation Manager
//  2. check git, then clone the wendy-lite tag or branch (see liteBuildRef)
//     from GitHub into a temporary folder
//  3. ask the configurator questions of the clone's master configuration;
//     the answers of the last run are the defaults
//  4. ask for Wi-Fi, the device name and Cloud enrollment: the content of
//     the wendy_conf partition
//  5. install the ESP-IDF version that the answers need
//  6. run tools/configure.py with the answers, then idf.py build
//  7. idf.py merge-bin, and write wendy_conf into the merged image
//  8. flash it as the catalog firmware is flashed, then enroll
func runLiteBuild(ctx context.Context, nightly bool, version string, wifi wifiCLIOptions, deviceName string, preOpts preEnrollOptions) error {
	if jsonOutput || !isInteractiveTerminal() {
		return usageErrorf("the Wendy Lite build asks questions: run it in an interactive terminal, without --json")
	}

	if err := ensureLiteBuildEIM(ctx); err != nil {
		return err
	}

	if err := checkGit(); err != nil {
		return err
	}
	ref := liteBuildRef(nightly, version)
	dir, commit, err := cloneLiteSources(ctx, espidftoolchain.WendyLiteGitURL, ref)
	if err != nil {
		return err
	}
	// The folder is kept once the build starts: its output and logs help
	// when a step fails, and it holds the two sdkconfig files to compare.
	keep := false
	defer func() {
		if keep {
			fmt.Printf("\nBuild folder: %s\n", dir)
		} else {
			os.RemoveAll(dir)
		}
	}()

	// Questions, from the master configuration of the clone: the code that
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
	// Not used by the build: kept to compare with configure.py's
	// sdkconfig.wendy.
	sdkconfigText := model.SdkconfigText(model.Header(result.Inputs), result.Inputs, result.Solution)
	if err := os.WriteFile(filepath.Join(dir, "sdkconfig.wendy.go"), []byte(sdkconfigText), 0o644); err != nil {
		return err
	}

	// Device configuration: the wendy_conf partition.
	prov, err := resolveLiteProvisioning(wifi, deviceName, preOpts)
	if err != nil {
		return err
	}
	payload, err := wendyConfPayload(prov.conf)
	if err != nil {
		return fmt.Errorf("serializing device config: %w", err)
	}
	// 0o600: it holds the Wi-Fi password.
	if err := os.WriteFile(filepath.Join(dir, "wendy_conf.bin"), payload, 0o600); err != nil {
		return err
	}

	fmt.Println()
	if err := espidftoolchain.EnsureIDF(ctx, build.eimVersion); err != nil {
		return err
	}

	keep = true
	configureArgs := append([]string{"tools/configure.py"}, build.configure...)
	if err := runLiteBuildCommand(ctx, dir, build.eimVersion, "python", configureArgs...); err != nil {
		return fmt.Errorf("tools/configure.py: %w", err)
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
	imagePath := filepath.Join(dir, "wendy-lite.bin")
	if err := os.WriteFile(imagePath, img.Bytes(), 0o600); err != nil {
		return err
	}
	fmt.Printf("\nBuilt Wendy Lite %s (%s) for the %s: %s\n", ref, commit, chipModelName(chip), imagePath)

	port, autoDiscover, err := detectLiteInstallPort(discovery.SerialPortInfo{}, prov, true)
	if err != nil {
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

// checkGit checks that git is installed and on PATH.
func checkGit() error {
	if _, err := liteBuildLookPath("git"); err != nil {
		return fmt.Errorf("git is not installed or not on PATH; %s", gitInstallHint(runtime.GOOS))
	}
	return nil
}

// gitInstallHint names the command that installs git on goos.
func gitInstallHint(goos string) string {
	switch goos {
	case "darwin":
		return "install it via 'xcode-select --install' or 'brew install git'"
	case "windows":
		return "install it via 'winget install --id Git.Git -e --source winget'"
	}
	return "install the git package of your distribution (for example 'sudo apt install git')"
}

// cloneLiteSources clones the tag or branch ref of the wendy-lite repository
// at url into a new temporary folder, without history. It returns the folder
// and the short commit.
func cloneLiteSources(ctx context.Context, url, ref string) (dir, commit string, err error) {
	dir, err = os.MkdirTemp("", "wendy-lite-build-")
	if err != nil {
		return "", "", err
	}
	fmt.Printf("Cloning wendy-lite %s from %s into %s...\n", ref, url, dir)
	// --branch takes a tag or a branch.
	if _, err := liteBuildGit(ctx, "", "clone", "--quiet", "--depth", "1", "--branch", ref, url, dir); err != nil {
		os.RemoveAll(dir)
		return "", "", fmt.Errorf("cloning wendy-lite %q (check the tag or branch name): %w", ref, err)
	}
	out, err := liteBuildGit(ctx, dir, "rev-parse", "--short", "HEAD")
	if err != nil {
		os.RemoveAll(dir)
		return "", "", err
	}
	return dir, strings.TrimSpace(out), nil
}

// liteBuildGit runs git, in dir when it is not empty, and returns its
// output.
func liteBuildGit(ctx context.Context, dir string, args ...string) (string, error) {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	// Fail instead of waiting for credentials: the repository is public.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if details := strings.TrimSpace(stderr.String()); details != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, details)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
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
