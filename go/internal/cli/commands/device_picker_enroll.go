package commands

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

var (
	connectPickerEnrollmentFn    = connectLocalPickerChoice
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

	target, err := connectPickerEnrollmentFn(ctx, item, suppressUpdateCheck)
	if err != nil {
		return err
	}
	defer target.Close()

	// The selected LAN connection retains its device pin checks. If dialing
	// fell back to BLE, reject it just as device enroll does: enrollment needs
	// the agent's gRPC provisioning service.
	conn, err := connectFromSelectedDevice(target, resolveConfig{suppressProvisioningHint: true})
	if err != nil {
		return err
	}
	promptPickerEnrollmentWifiFn(ctx, conn)
	return runPickerEnrollmentFn(ctx, conn, auth, "", 0)
}
