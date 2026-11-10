package commands

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

var (
	connectPickerEnrollmentFn = func(ctx context.Context, item *tui.PickerItem, suppressUpdateCheck bool) (*SelectedDevice, error) {
		return connectLocalPickerChoice(withUnboundSetupSelection(ctx), item, suppressUpdateCheck)
	}
	promptPickerEnrollmentWifiFn = promptWifiIfNeeded
	runPickerEnrollmentFn        = runEnrollDevice
)

// enrollLocalPickerDevice runs device enroll after the picker has released the
// terminal. Resolve the captured row directly: consulting --device or the saved
// default here could enroll a different device than the one the user highlighted.
func enrollLocalPickerDevice(ctx context.Context, item *tui.PickerItem, auth *config.AuthConfig, suppressUpdateCheck bool) error {
	if auth == nil {
		return config.ErrNotLoggedIn
	}
	if len(auth.Certificates) == 0 {
		return fmt.Errorf("selected auth entry has no certificates; re-run 'wendy auth login'")
	}

	return runWithEnrollmentTarget(ctx, func() (*SelectedDevice, error) {
		return connectPickerEnrollmentFn(ctx, item, suppressUpdateCheck)
	}, func(target *SelectedDevice) error {
		conn, err := connectFromSelectedDevice(target, resolveConfig{suppressProvisioningHint: true})
		if err != nil {
			return err
		}
		// The Wi-Fi prompt asks before sending a password over an unverified
		// setup connection.
		if err := promptPickerEnrollmentWifiFn(context.WithValue(ctx, wifiCredentialTargetKey{}, target), conn); err != nil {
			return err
		}
		if target.recordEnrollmentBinding != nil {
			ctx = context.WithValue(ctx, enrollmentBindingRecorderKey{}, target.recordEnrollmentBinding)
		}
		return runPickerEnrollmentFn(ctx, conn, auth, "", 0)
	})
}
