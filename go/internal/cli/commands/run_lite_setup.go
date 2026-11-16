package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/espidftoolchain"
	"github.com/wendylabsinc/wendy/go/internal/cli/litebuild"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
)

// liteBuildRepair returns the repair that offers to fix what a failed
// build reports, or nil when err is not one that a repair fixes. kind names
// the repair, so that providerBuild runs each one at most once. A repair
// reports whether the build should run again; a user who says no to a
// project change gets ErrUserCancelled.
//
// The repairs follow the order of the checks of an ESP-IDF build for Wendy
// Lite: EIM, sdkconfig.defaults.wendy, the SDKCONFIG_DEFAULTS of
// CMakeLists.txt, the wendy_core version, a build folder of another ESP-IDF
// version, then the wendy_core component.
func liteBuildRepair(err error, projectPath string, assumeYes bool) (kind string, repair func(context.Context) (bool, error)) {
	var appDefaults *providers.LiteAppDefaultsError
	var defaultsList *providers.MissingSdkconfigDefaultsListError
	var coreVersion *providers.WendyCoreVersionError
	var missingCore *providers.MissingWendyCoreError
	var idfChanged *providers.IDFVersionChangedError
	switch {
	case errors.Is(err, espidftoolchain.ErrEIMNotInstalled):
		return "eim", func(ctx context.Context) (bool, error) {
			return installMissingEIM(ctx, assumeYes)
		}
	case errors.As(err, &appDefaults):
		return "app defaults", func(ctx context.Context) (bool, error) {
			return true, setupLiteAppDefaults(ctx, projectPath, appDefaults, assumeYes)
		}
	case errors.As(err, &defaultsList):
		return "defaults list", func(context.Context) (bool, error) {
			return true, setupSdkconfigDefaultsList(projectPath, assumeYes)
		}
	case errors.As(err, &coreVersion):
		return "wendy_core version", func(context.Context) (bool, error) {
			return true, setupWendyCoreVersion(projectPath, coreVersion, assumeYes)
		}
	case errors.As(err, &idfChanged):
		return "ESP-IDF version", func(ctx context.Context) (bool, error) {
			return true, cleanBuildForIDF(ctx, projectPath, idfChanged, assumeYes)
		}
	case errors.As(err, &missingCore):
		return "wendy_core", func(ctx context.Context) (bool, error) {
			return true, setupLiteProject(ctx, projectPath, missingCore, assumeYes)
		}
	}
	return "", nil
}

func setupLiteProject(ctx context.Context, projectPath string, missing *providers.MissingWendyCoreError, assumeYes bool) error {
	setup, err := espidftoolchain.PrepareWendyCore(projectPath, missing.Toolchain, missing.Version)
	if err != nil {
		return err
	}
	cliLogln("This project needs wendy_core to run on Wendy Lite.")
	if missing.Version != "" {
		cliLogln("Wendy will add it to main/idf_component.yml, at the Wendy Lite version of the device (%s), and initialize it first in app_main() in %s.", litebuild.ShortVersion(missing.Version), setup.SourcePath())
	} else {
		cliLogln("Wendy will add it to main/idf_component.yml and initialize it first in app_main() in %s.", setup.SourcePath())
	}
	if !assumeYes && !confirmFn("Add wendy_core and its initialization now?") {
		return ErrUserCancelled
	}
	if err := setup.Apply(ctx); err != nil {
		return fmt.Errorf("setting up wendy_core: %w", err)
	}
	cliLogln("Wendy Core configured.")
	return nil
}

// fetchMasterConfFn is a seam for tests.
var fetchMasterConfFn = litebuild.FetchMasterConf

// setupLiteAppDefaults offers to write sdkconfig.defaults.wendy for the
// Wendy Lite version and the build profile of the device: it fetches the
// master configuration of that version from the wendy-lite repository and
// solves the build profile.
func setupLiteAppDefaults(ctx context.Context, projectPath string, e *providers.LiteAppDefaultsError, assumeYes bool) error {
	const file = litebuild.AppDefaultsFile
	short := litebuild.ShortVersion(e.Version)
	question := "Update " + file + " now?"
	if e.Status.State == litebuild.AppDefaultsMissing {
		cliLogln("This project has no %s, the configuration of the Wendy Lite build on the device (Wendy Lite %s; %s).", file, short, profileText(e.Profile))
		question = "Create " + file + " now?"
	} else {
		fileVersion := "an unknown version"
		if e.Status.Version != "" {
			fileVersion = "Wendy Lite " + litebuild.ShortVersion(e.Status.Version)
		}
		cliLogln("%s is for %s (%s), but the device runs Wendy Lite %s (%s).", file, fileVersion, profileText(e.Status.Profile), short, profileText(e.Profile))
	}
	cliLogln("Wendy will write it from %s of wendy-lite %s.", litebuild.MasterConfFile, short)
	if _, err := os.Stat(filepath.Join(projectPath, "sdkconfig")); err == nil {
		cliLogln("The build then generates sdkconfig again and keeps the current one as sdkconfig.old.")
	}
	if !assumeYes && !confirmFn(question) {
		return ErrUserCancelled
	}

	model, err := fetchMasterConfFn(ctx, espidftoolchain.WendyLiteGitURL, espidftoolchain.WendyLiteGitRef(e.Version), cliLogln)
	if err != nil {
		return err
	}
	text, err := litebuild.AppDefaultsText(model, e.Version, e.Profile, e.Target)
	if err != nil {
		return fmt.Errorf("writing %s: %w", file, err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, file), []byte(text), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", file, err)
	}
	cliLogln("%s written.", file)
	return nil
}

// profileText returns a build profile on one line, for the messages.
func profileText(profile string) string {
	var parts []string
	for _, line := range strings.Split(profile, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	if len(parts) == 0 {
		return "no build profile"
	}
	return strings.Join(parts, ", ")
}

// setupSdkconfigDefaultsList offers to make CMakeLists.txt list
// sdkconfig.defaults.wendy in SDKCONFIG_DEFAULTS, and to create the
// sdkconfig.defaults that the list names when the project has none.
func setupSdkconfigDefaultsList(projectPath string, assumeYes bool) error {
	setup, err := espidftoolchain.PrepareSdkconfigDefaultsList(projectPath)
	if err != nil || setup == nil {
		return err
	}
	var question string
	if setup.AddsLine() {
		cliLogln("The build must use %s: Wendy will add this line to CMakeLists.txt, before include($ENV{IDF_PATH}/tools/cmake/project.cmake):", litebuild.AppDefaultsFile)
		cliLogln("  %s", espidftoolchain.SdkconfigDefaultsLine)
		question = "Add it to CMakeLists.txt now?"
	}
	if setup.CreateDefaults {
		cliLogln("SDKCONFIG_DEFAULTS also names sdkconfig.defaults, which this project does not have: Wendy will create it with a comment line.")
		question = "Create sdkconfig.defaults now?"
		if setup.AddsLine() {
			question = "Add the line to CMakeLists.txt and create sdkconfig.defaults now?"
		}
	}
	if !assumeYes && !confirmFn(question) {
		return ErrUserCancelled
	}
	if err := setup.Apply(); err != nil {
		return err
	}
	cliLogln("SDKCONFIG_DEFAULTS configured.")
	return nil
}

// setupWendyCoreVersion offers to pin the wendy_core dependency to the
// Wendy Lite version of the device.
func setupWendyCoreVersion(projectPath string, e *providers.WendyCoreVersionError, assumeYes bool) error {
	setup, _, err := espidftoolchain.PrepareWendyCoreVersion(projectPath, e.Version)
	if err != nil || setup == nil {
		return err
	}
	short := litebuild.ShortVersion(e.Version)
	cliLogln("main/idf_component.yml uses %s of wendy_core, but the device runs Wendy Lite %s.", providers.WendyCoreVersionText(setup.Current), short)
	cliLogln("Wendy will set version: %q in the wendy_core dependency of main/idf_component.yml, and rebuild dependencies.lock. Other dependencies in dependencies.lock may be updated as well.", setup.Version)
	if !assumeYes && !confirmFn("Update main/idf_component.yml and rebuild dependencies.lock now?") {
		return ErrUserCancelled
	}
	if err := setup.Apply(); err != nil {
		return err
	}
	cliLogln("wendy_core dependency changed.")
	return nil
}

// cleanBuildForIDF offers to clean the build folder that another ESP-IDF
// version configured, with idf.py fullclean in the ESP-IDF version of the
// build.
func cleanBuildForIDF(ctx context.Context, projectPath string, e *providers.IDFVersionChangedError, assumeYes bool) error {
	cliLogln("The build folder was configured with ESP-IDF %s, but the Wendy Lite build of the device uses ESP-IDF %s.", e.Configured, e.Version)
	cliLogln("Wendy will clean it with idf.py fullclean. The build then configures the project again.")
	if !assumeYes && !confirmFn("Clean the build folder with idf.py fullclean now?") {
		return ErrUserCancelled
	}
	if err := e.Toolchain.Run(ctx, projectPath, "fullclean"); err != nil {
		return fmt.Errorf("idf.py fullclean: %w", err)
	}
	cliLogln("Build folder cleaned.")
	return nil
}

// Seams so tests never run Homebrew or depend on the host's eim.
var (
	eimLookPathFn = exec.LookPath
	installEIMFn  = func(ctx context.Context) error {
		return brewInstallWithSpinner(ctx, espidftoolchain.EIMFormula, "Installing EIM via Homebrew...")
	}
)

// installMissingEIM offers to install eim, which ESP-IDF builds run through,
// with Homebrew. It reports false without Homebrew or when the user declines,
// leaving the build's error to name the manual install.
func installMissingEIM(ctx context.Context, assumeYes bool) (bool, error) {
	if _, err := brewLookPathFn("brew"); err != nil {
		return false, nil
	}
	if !assumeYes {
		cliLogln("ESP-IDF builds run through EIM, the ESP-IDF Installation Manager, which is not installed. Wendy can install it with `brew install %s`.", espidftoolchain.EIMFormula)
		if !confirmFn("Do you want to install EIM?") {
			return false, nil
		}
	}
	if err := installEIMFn(ctx); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if errors.Is(err, ErrUserCancelled) {
			return false, err
		}
		return false, fmt.Errorf("installing EIM via Homebrew: %w", err)
	}
	if _, err := eimLookPathFn("eim"); err != nil {
		return false, errors.New("EIM was installed via Homebrew but is not yet on PATH; " +
			"open a new terminal or run: eval \"$(brew shellenv)\"")
	}
	cliLogln("EIM installed.")
	return true, nil
}
