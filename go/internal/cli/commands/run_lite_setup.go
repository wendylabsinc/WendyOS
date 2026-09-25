package commands

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/cli/espidftoolchain"
)

func setupLiteProject(ctx context.Context, projectPath string, assumeYes bool) error {
	setup, err := espidftoolchain.PrepareWendyCore(projectPath)
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
