package commands

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/wendylabsinc/wendy/go/internal/cli/espidftoolchain"
)

func setupLiteProject(ctx context.Context, projectPath string, toolchain *espidftoolchain.ProjectToolchain, assumeYes bool) error {
	setup, err := espidftoolchain.PrepareWendyCore(projectPath, toolchain)
	if err != nil {
		return err
	}
	cliLogln("This project needs wendy_core to run on Wendy Lite.")
	cliLogln("Wendy will add it to main/idf_component.yml and initialize it first in app_main() in %s.", setup.SourcePath())
	if !assumeYes && !confirmFn("Add wendy_core and its initialization now?") {
		return ErrUserCancelled
	}
	if err := setup.Apply(ctx); err != nil {
		return fmt.Errorf("setting up wendy_core: %w", err)
	}
	cliLogln("Wendy Core configured. Retrying the build...")
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
	cliLogln("EIM installed. Retrying the build...")
	return true, nil
}
