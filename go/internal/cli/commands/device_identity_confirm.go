package commands

import (
	"errors"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// The identity checks themselves (device_pin.go, dial_target.go) always
// refuse a changed identity. Only a command's front door, with a person at a
// terminal, may ask whether to trust it (owner ruling 2026-11-13: "Treat it
// SSH style and ask for confirmation").

var confirmChangedIdentityFn = func() (bool, error) {
	return tui.ConfirmDefaultNo("Trust the new identity and connect?", tea.WithOutput(os.Stderr))
}

// acceptChangedIdentity answers a changed device identity the way SSH answers a
// changed host key: explain, ask, default no. retry reports that the user
// accepted and the new identity is saved, so the caller connects again once;
// otherwise err is what the caller returns. Without a terminal it never
// accepts: the refusal already says to run the command once interactively.
func acceptChangedIdentity(err error, nonInteractive bool) (retry bool, _ error) {
	var refusal *deviceIdentityRefusalError
	if !errors.As(err, &refusal) || refusal.accept == nil || refusal.diagnostic == nil {
		return false, err
	}
	if nonInteractive || jsonOutput || !isInteractiveTerminal() {
		return false, err
	}
	fmt.Fprintln(os.Stderr, refusal.diagnostic.changedIdentityWarning(true))
	accepted, promptErr := confirmChangedIdentityFn()
	if errors.Is(promptErr, tui.ErrCancelled) || (promptErr == nil && !accepted) {
		return false, fmt.Errorf("%w: the saved identity for %q was kept", ErrUserCancelled, refusal.diagnostic.hostname)
	}
	if promptErr != nil {
		return false, promptErr
	}
	if saveErr := refusal.accept(); saveErr != nil {
		return false, fmt.Errorf("saving the new identity for %q: %w", refusal.diagnostic.hostname, saveErr)
	}
	// A broker may still hold a transport authenticated under the old identity.
	_ = sessionbroker.InvalidateAll()
	return true, nil
}

// saveChangedIdentity replaces the saved identity for key with obs, clearing
// whatever the old one left behind (as `wendy device unpin` does).
func saveChangedIdentity(key string, obs observedDeviceIdentity) func() error {
	return func() error {
		return config.Update(func(cfg *config.Config) (bool, error) {
			clearPinsGoverning(cfg, key)
			cfg.SetDevicePin(key, obs.orgID, cloudGRPCForIdentity(cfg, obs), obs.assetID, obs.principal)
			return true, nil
		})
	}
}
