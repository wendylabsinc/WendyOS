//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

var createTourVMFn = func(ctx context.Context, out io.Writer, name string) error {
	return createVMContext(ctx, out, name, "", "", defaultSimulatorDiskGiB, false, 0)
}

func ensureTourVM(ctx context.Context, out io.Writer, name string) (runErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	store, err := robotVMStore()
	if err != nil {
		return err
	}
	// Check the profile before recovery as well as afterward, so a starter
	// edited to point at another robot never replaces that robot's VM.
	if _, err := tourVMHasGo2Profile(store, name); err != nil {
		return err
	}
	ready, preservedAt, err := store.PrepareForReuse(name)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !ready {
		if preservedAt != "" {
			fmt.Fprintln(out, "Resuming interrupted setup. Previous files are saved.")
			defer func() {
				if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, ErrUserCancelled) {
					runErr = fmt.Errorf("%w; previous setup files saved at %s", runErr, preservedAt)
				}
			}()
		}
		if err := createTourVMFn(ctx, io.Discard, name); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	hasProfile, err := tourVMHasGo2Profile(store, name)
	if err != nil || hasProfile {
		return err
	}
	if err := attachSimulatorProfileQuiet(name, vm.RobotKindGo2); err != nil {
		// Another tour invocation can attach the same profile after our read.
		if errors.Is(err, vm.ErrRobotProfileExists) {
			_, err = tourVMHasGo2Profile(store, name)
		}
		return err
	}
	return nil
}

func tourVMHasGo2Profile(store *vm.Store, name string) (bool, error) {
	profile, exists, err := store.ReadRobotProfile(name)
	if err != nil {
		return false, err
	}
	if exists && profile.Kind != vm.RobotKindGo2 {
		return false, errors.New("this starter's VM has a different robot profile; choose a new --project directory")
	}
	return exists, nil
}
