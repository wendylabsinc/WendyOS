package commands

import (
	"errors"
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Text signals used to classify deploy-path failures caused by device
// storage exhaustion (WDY-3127). Matched on error/output text only — never
// syscall.ENOSPC — because the CLI cross-compiles for Windows, where that
// constant does not exist.
const (
	// outOfSpaceSignal is the kernel's own ENOSPC wording, as it reaches the
	// CLI verbatim through containerd/buildkit error chains.
	outOfSpaceSignal = "no space left on device"
	// storageDegradedCodeSignal is the agent registry's HTTP 507 error code
	// (WDY-3127), as go-containerregistry/buildx render it: "POST
	// .../blobs/uploads/: WENDY_STORAGE_DEGRADED: <message>".
	storageDegradedCodeSignal = "WENDY_STORAGE_DEGRADED"
	// storageDegradedTicketSignal is the suffix the agent's gRPC
	// FailedPrecondition rejection ends every message with (WDY-3127).
	storageDegradedTicketSignal = "(WDY-3127)"
)

// isDeviceOutOfSpace reports whether err's text indicates the device ran out
// of disk space while ingesting a build or writing container/provisioning
// state.
func isDeviceOutOfSpace(err error) bool {
	return err != nil && strings.Contains(err.Error(), outOfSpaceSignal)
}

// isContainerStorageDegradedError reports whether err is, wraps, or
// stringifies to a container-storage-degraded rejection: the CLI's own
// preflight error type (containerStorageDegradedError), the agent registry's
// HTTP 507 WENDY_STORAGE_DEGRADED code, or the agent's gRPC FailedPrecondition
// text (which always ends in "(WDY-3127)").
func isContainerStorageDegradedError(err error) bool {
	if err == nil {
		return false
	}
	var degraded *containerStorageDegradedError
	if errors.As(err, &degraded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, storageDegradedCodeSignal) || strings.Contains(msg, storageDegradedTicketSignal)
}

// isNonRetryablePushOutput reports whether buildx/push output contains
// either text signal above. Retrying a push that failed for one of these
// reasons would only fail again identically, so shouldRetryPush uses this to
// stop the retry loop early.
func isNonRetryablePushOutput(output string) bool {
	return strings.Contains(output, outOfSpaceSignal) ||
		strings.Contains(output, storageDegradedCodeSignal) ||
		strings.Contains(output, storageDegradedTicketSignal)
}

// describeDeployStorageFailure classifies a deploy-path failure and, when it
// is storage-related, replaces it with an explanation and the right remedy
// instead of the raw error: power-cycle guidance when container storage is
// degraded (stuck on the OS root slot, WDY-3127), or a cache-prune suggestion
// for a plain full disk. resp is nil-safe. Unrelated errors are returned
// unchanged; storage-related errors are wrapped with %w so errors.Is still
// finds the original.
func describeDeployStorageFailure(err error, resp *agentpb.GetAgentVersionResponse) error {
	if err == nil {
		return nil
	}

	if isContainerStorageDegradedError(err) || (isDeviceOutOfSpace(err) && containerStorageDegraded(resp)) {
		degraded := &containerStorageDegradedError{storage: resp.GetContainerStorage()}
		return fmt.Errorf("%s Original error: %w", degraded.Error(), err)
	}

	if isDeviceOutOfSpace(err) {
		const remedy = "Free space with 'wendy device cache prune --all' " +
			"(releases cached layers not used by any deployed app; deployed apps are kept), then rerun."
		storage := resp.GetContainerStorage()
		if storage == nil || storage.GetDevice() == "" || storage.GetTotalBytes() <= 0 {
			return fmt.Errorf("The device's container storage is full. %s Original error: %w", remedy, err)
		}
		return fmt.Errorf("The device's container storage is full: %s (%s) %s of %s used. %s Original error: %w",
			storage.GetMountpoint(), storage.GetDevice(),
			formatGigabytes(storage.GetUsedBytes()), formatGigabytes(storage.GetTotalBytes()),
			remedy, err)
	}

	return err
}
