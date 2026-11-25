package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// powerServiceHarness drives PowerService without a real clock or reboot: the
// delayed reboot is captured and run by the test.
type powerServiceHarness struct {
	svc       *PowerService
	installer *AgentInstaller
	scheduled []func()
	delays    []time.Duration
	reboots   int
	modes     []agentpbv2.RebootMode
	rebootErr error
}

func newPowerServiceHarness(t *testing.T) *powerServiceHarness {
	t.Helper()
	h := &powerServiceHarness{installer: &AgentInstaller{}}
	h.svc = NewPowerService(zap.NewNop(), h.installer)
	h.svc.supported = func() bool { return true }
	h.svc.hasForcedRecovery = func() bool { return true }
	h.svc.reboot = func(mode agentpbv2.RebootMode) error {
		h.reboots++
		h.modes = append(h.modes, mode)
		return h.rebootErr
	}
	h.svc.schedule = func(delay time.Duration, fn func()) {
		h.delays = append(h.delays, delay)
		h.scheduled = append(h.scheduled, fn)
	}
	return h
}

func (h *powerServiceHarness) request(t *testing.T) error {
	t.Helper()
	return h.requestMode(t, agentpbv2.RebootMode_REBOOT_MODE_UNSPECIFIED)
}

func (h *powerServiceHarness) requestMode(t *testing.T, mode agentpbv2.RebootMode) error {
	t.Helper()
	_, err := h.svc.Reboot(context.Background(), &agentpbv2.RebootRequest{Mode: mode})
	return err
}

func TestRebootAnswersBeforeTheDeviceGoesDown(t *testing.T) {
	h := newPowerServiceHarness(t)
	if err := h.request(t); err != nil {
		t.Fatalf("Reboot() = %v, want success", err)
	}
	if h.reboots != 0 {
		t.Fatalf("device was rebooted %d times before the response was returned", h.reboots)
	}
	if len(h.scheduled) != 1 {
		t.Fatalf("scheduled %d reboots, want 1", len(h.scheduled))
	}
	if h.delays[0] != rebootRequestDelay || rebootRequestDelay <= 0 {
		t.Fatalf("reboot delay = %v, want the positive rebootRequestDelay %v so the response can reach the caller", h.delays[0], rebootRequestDelay)
	}
	h.scheduled[0]()
	if h.reboots != 1 {
		t.Fatalf("device was rebooted %d times after the delay, want 1", h.reboots)
	}
}

func TestRebootIsRefusedOnAHostTheAgentCannotReboot(t *testing.T) {
	h := newPowerServiceHarness(t)
	h.svc.supported = func() bool { return false }
	if err := h.request(t); status.Code(err) != codes.Unavailable {
		t.Fatalf("Reboot() code = %v, want Unavailable (err=%v)", status.Code(err), err)
	}
	if len(h.scheduled) != 0 {
		t.Fatal("a reboot was scheduled on a host that cannot be rebooted")
	}
	if !h.installer.TryLock() {
		t.Fatal("a refused reboot left updates locked out")
	}
}

// A reboot while an agent or OS update is being written would interrupt it.
func TestRebootIsRefusedWhileAnUpdateIsBeingInstalled(t *testing.T) {
	h := newPowerServiceHarness(t)
	if !h.installer.TryLock() {
		t.Fatal("could not take the install lock")
	}
	if err := h.request(t); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Reboot() code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if len(h.scheduled) != 0 {
		t.Fatal("a reboot was scheduled while an update was being installed")
	}
}

// Once a reboot is on its way, an update must not start underneath it.
func TestAcceptedRebootKeepsUpdatesOut(t *testing.T) {
	h := newPowerServiceHarness(t)
	if err := h.request(t); err != nil {
		t.Fatalf("Reboot() = %v, want success", err)
	}
	if h.installer.TryLock() {
		t.Fatal("an update could start while a reboot was pending")
	}
}

// A second request while the first is pending is the same reboot, not an error
// and not a second one.
func TestRepeatedRebootRequestIsOneReboot(t *testing.T) {
	h := newPowerServiceHarness(t)
	for i := 0; i < 2; i++ {
		if err := h.request(t); err != nil {
			t.Fatalf("Reboot() call %d = %v, want success", i+1, err)
		}
	}
	if len(h.scheduled) != 1 {
		t.Fatalf("scheduled %d reboots for two requests, want 1", len(h.scheduled))
	}
}

// When the host refuses the reboot, the device stays up, so it must stay
// usable: updates are allowed again and a later request tries again.
func TestFailedRebootLeavesTheDeviceUsable(t *testing.T) {
	h := newPowerServiceHarness(t)
	h.rebootErr = errors.New("systemctl: access denied")
	if err := h.request(t); err != nil {
		t.Fatalf("Reboot() = %v, want success", err)
	}
	h.scheduled[0]()
	if h.reboots != 1 {
		t.Fatalf("reboot attempted %d times, want 1", h.reboots)
	}
	if !h.installer.TryLock() {
		t.Fatal("a failed reboot left updates locked out")
	}
	h.installer.Unlock()

	h.rebootErr = nil
	if err := h.request(t); err != nil {
		t.Fatalf("Reboot() after a failed reboot = %v, want success", err)
	}
	if len(h.scheduled) != 2 {
		t.Fatalf("scheduled %d reboots, want a second attempt", len(h.scheduled))
	}
}

const forcedRecovery = agentpbv2.RebootMode_REBOOT_MODE_FORCED_RECOVERY

func TestRebootIntoForcedRecoveryReachesTheHost(t *testing.T) {
	h := newPowerServiceHarness(t)
	if err := h.requestMode(t, forcedRecovery); err != nil {
		t.Fatalf("Reboot(forced recovery) = %v, want success", err)
	}
	if len(h.scheduled) != 1 {
		t.Fatalf("scheduled %d reboots, want 1", len(h.scheduled))
	}
	h.scheduled[0]()
	if len(h.modes) != 1 || h.modes[0] != forcedRecovery {
		t.Fatalf("host was asked for %v, want one reboot into forced recovery", h.modes)
	}
}

func TestPlainRebootRequestIsANormalReboot(t *testing.T) {
	h := newPowerServiceHarness(t)
	if err := h.request(t); err != nil {
		t.Fatalf("Reboot() = %v, want success", err)
	}
	h.scheduled[0]()
	if len(h.modes) != 1 || h.modes[0] != agentpbv2.RebootMode_REBOOT_MODE_UNSPECIFIED {
		t.Fatalf("host was asked for %v, want one normal reboot", h.modes)
	}
}

// A board without forced-recovery mode would ignore the kernel argument and
// boot normally, so the caller would wait for a device that is back online.
func TestForcedRecoveryIsRefusedOnABoardWithoutIt(t *testing.T) {
	h := newPowerServiceHarness(t)
	h.svc.hasForcedRecovery = func() bool { return false }
	err := h.requestMode(t, forcedRecovery)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Reboot(forced recovery) code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if !strings.Contains(status.Convert(err).Message(), "Jetson") {
		t.Fatalf("refusal %q does not say which boards have the mode", status.Convert(err).Message())
	}
	if len(h.scheduled) != 0 {
		t.Fatal("a reboot was scheduled on a board without forced-recovery mode")
	}
	if !h.installer.TryLock() {
		t.Fatal("a refused reboot left updates locked out")
	}
	h.installer.Unlock()
	if err := h.request(t); err != nil {
		t.Fatalf("normal Reboot() on the same board = %v, want success", err)
	}
}

// A mode from a newer CLI must not turn into a normal reboot on this agent.
func TestUnknownRebootModeIsRefused(t *testing.T) {
	h := newPowerServiceHarness(t)
	err := h.requestMode(t, agentpbv2.RebootMode(99))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Reboot(mode 99) code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
	if len(h.scheduled) != 0 {
		t.Fatal("a reboot was scheduled for an unknown mode")
	}
	if !h.installer.TryLock() {
		t.Fatal("a refused reboot left updates locked out")
	}
}

// A request for another mode while one reboot is pending cannot be honoured:
// the device goes down the way the first request asked.
func TestRebootForAnotherModeWhileOneIsPendingIsRefused(t *testing.T) {
	h := newPowerServiceHarness(t)
	if err := h.request(t); err != nil {
		t.Fatalf("Reboot() = %v, want success", err)
	}
	err := h.requestMode(t, forcedRecovery)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Reboot(forced recovery) while a normal reboot is pending: code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if !strings.Contains(status.Convert(err).Message(), "normal reboot is already under way") {
		t.Fatalf("refusal %q does not name the pending reboot", status.Convert(err).Message())
	}
	if err := h.request(t); err != nil {
		t.Fatalf("repeated normal Reboot() = %v, want success", err)
	}
	if len(h.scheduled) != 1 {
		t.Fatalf("scheduled %d reboots, want only the first", len(h.scheduled))
	}
}

// On a Closed device forced recovery is owner only; a normal reboot stays
// operator, and Open/Enrolled devices (no caller role) are unchanged.
func TestForcedRecoveryNeedsTheOwnerOnAClosedDevice(t *testing.T) {
	for _, role := range []trustedkeys.Role{trustedkeys.RoleNone, trustedkeys.RoleViewer, trustedkeys.RoleOperator} {
		h := newPowerServiceHarness(t)
		ctx := trustedkeys.WithCallerRole(context.Background(), role)
		_, err := h.svc.Reboot(ctx, &agentpbv2.RebootRequest{Mode: forcedRecovery})
		if status.Code(err) != codes.PermissionDenied || len(h.scheduled) != 0 {
			t.Fatalf("%v: forced recovery = %v scheduled=%d, want PermissionDenied", role, err, len(h.scheduled))
		}
		if _, err := h.svc.Reboot(ctx, &agentpbv2.RebootRequest{}); err != nil {
			t.Fatalf("%v: normal reboot = %v", role, err)
		}
	}
	h := newPowerServiceHarness(t)
	if _, err := h.svc.Reboot(trustedkeys.WithCallerRole(context.Background(), trustedkeys.RoleOwner), &agentpbv2.RebootRequest{Mode: forcedRecovery}); err != nil {
		t.Fatalf("owner forced recovery = %v", err)
	}
}
