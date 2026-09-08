package containerd

import (
	"errors"
	"fmt"
	"testing"
)

// The whole point of the reason: a GPU app whose device has gone must not land
// in the same bucket as an app with a bug in it. "crashed, exit 139" is what
// sent a week of debugging in the wrong direction.
func TestClassifyStartError_DeviceUnavailableIsItsOwnReason(t *testing.T) {
	err := fmt.Errorf("%w: myapp names /dev/nvgpu/igpu0/ctrl, absent on this host", ErrDeviceUnavailable)

	if got := classifyStartError(err); got != exitReasonDeviceUnavailable {
		t.Errorf("classifyStartError = %q; want %q", got, exitReasonDeviceUnavailable)
	}
	if got := classifyStartError(err); got == exitReasonCrashed {
		t.Error("a missing device classified as a crash; that is the confusion this exists to end")
	}
}

// The existing classifications must be untouched.
func TestClassifyStartError_OtherReasonsUnchanged(t *testing.T) {
	if got := classifyStartError(errors.New("applying entitlements: nope")); got != exitReasonEntitlementDenied {
		t.Errorf("entitlement error = %q; want %q", got, exitReasonEntitlementDenied)
	}
	if got := classifyStartError(errors.New("pulling image: not found")); got != exitReasonStartFailed {
		t.Errorf("generic start error = %q; want %q", got, exitReasonStartFailed)
	}
	if got := classifyStartError(nil); got != exitReasonStartFailed {
		t.Errorf("nil error = %q; want %q", got, exitReasonStartFailed)
	}
}

// An entitlement error that happens to mention a device must not be reclassified
// by accident: the sentinel is checked by identity, not by wording.
func TestClassifyStartError_MatchesSentinelNotWording(t *testing.T) {
	if got := classifyStartError(errors.New("device unavailable somewhere")); got != exitReasonStartFailed {
		t.Errorf("unwrapped lookalike = %q; want %q — only the sentinel counts", got, exitReasonStartFailed)
	}
}
