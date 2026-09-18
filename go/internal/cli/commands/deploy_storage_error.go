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
	// storageDegraded507Signal is the bare HTTP status line containerd's
	// pusher renders for the registry's 507 Insufficient Storage response on
	// the fused buildx build+push path: no WENDY_STORAGE_DEGRADED code and
	// no (WDY-3127) marker survive, only the status line itself.
	storageDegraded507Signal = "507 Insufficient Storage"
)

// isDeviceOutOfSpace reports whether err's text indicates the DEVICE ran out
// of disk space while ingesting a build or writing container/provisioning
// state. A bare "no space left on device" is not enough on its own: on the
// fused buildx build+push path, containerd's pusher renders any non-2xx
// registry response as an "unexpected status ..." line without the original
// body, so any device-side rejection text never survives — but a LOCAL
// BuildKit worker (e.g. a full Docker Desktop disk) can print that exact
// kernel wording directly. Requiring a device-side marker in addition
// avoids misattributing a local build failure to the device (WDY-3127 I1).
func isDeviceOutOfSpace(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, outOfSpaceSignal) {
		return false
	}
	if strings.Contains(msg, "rpc error: code =") {
		return true
	}
	if strings.Contains(msg, "pushing OCI layout to registry") {
		return true
	}
	return strings.Contains(msg, "/v2/") && strings.Contains(msg, "/blobs/uploads/")
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
	return strings.Contains(msg, storageDegradedCodeSignal) ||
		strings.Contains(msg, storageDegradedTicketSignal) ||
		strings.Contains(msg, storageDegraded507Signal)
}

// isNonRetryablePushOutput reports whether buildx/push output contains
// either text signal above. Retrying a push that failed for one of these
// reasons would only fail again identically, so shouldRetryPush uses this to
// stop the retry loop early.
func isNonRetryablePushOutput(output string) bool {
	return strings.Contains(output, outOfSpaceSignal) ||
		strings.Contains(output, storageDegradedCodeSignal) ||
		strings.Contains(output, storageDegradedTicketSignal) ||
		strings.Contains(output, storageDegraded507Signal)
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

	var typed *containerStorageDegradedError
	matchesTypedPreflight := errors.As(err, &typed)
	matchesDerivedENOSPC := isDeviceOutOfSpace(err) && containerStorageDegraded(resp)

	if matchesTypedPreflight || matchesDerivedENOSPC {
		// err's own text does not (yet) carry the explanation: it's either
		// the CLI's own bare preflight error type, or a generic ENOSPC
		// string that only means "root-slot degraded" once cross-referenced
		// against resp. Build the full message.
		degraded := &containerStorageDegradedError{storage: resp.GetContainerStorage()}
		return fmt.Errorf("%s Original error: %w", degraded.Error(), err)
	}

	if isContainerStorageDegradedError(err) {
		if strings.Contains(err.Error(), storageDegradedTicketSignal) {
			// err's text already came from the agent's own gRPC
			// FailedPrecondition description, which ends every message with
			// the full (WDY-3127) remedy sentence; prepending the CLI's copy
			// would just duplicate it (M4).
			return fmt.Errorf("deploy refused: %w", err)
		}
		// A bare WENDY_STORAGE_DEGRADED code (no message) or a bare 507
		// Insufficient Storage status line — containerd's pusher strips
		// everything else on the fused buildx build+push path, so no remedy
		// text survived. Give the full explanation instead of leaving the
		// user with an unexplained code (M4).
		degraded := &containerStorageDegradedError{storage: resp.GetContainerStorage()}
		return fmt.Errorf("%s Original error: %w", degraded.Error(), err)
	}

	if isDeviceOutOfSpace(err) {
		const remedy = "Free space with 'wendy device cache prune', then rerun."
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
