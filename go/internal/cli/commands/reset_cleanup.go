package commands

import "github.com/wendylabsinc/wendy/go/internal/shared/resetcleanup"

// resetCleanupRecoveryStep is what to do about a device that was reset but
// cannot finish removing what its previous enrollment left on the host. The
// agent retries every few seconds on its own; what it cannot remove is usually
// kernel state, which does not survive a reboot.
const resetCleanupRecoveryStep = "If this does not clear within a minute, restart the device: network rules and routes left over from before the reset are gone after a reboot."

// ResetCleanupRecovery returns the recovery step for an error that carries an
// agent's reset-cleanup refusal (see resetcleanup.Refusal), and nothing for
// any other error. The agent's own message already says what is failing.
func ResetCleanupRecovery(err error) []string {
	if _, ok := resetcleanup.Cause(err); !ok {
		return nil
	}
	return []string{resetCleanupRecoveryStep}
}
