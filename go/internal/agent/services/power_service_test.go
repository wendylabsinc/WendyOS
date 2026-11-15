package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
	rebootErr error
}

func newPowerServiceHarness(t *testing.T) *powerServiceHarness {
	t.Helper()
	h := &powerServiceHarness{installer: &AgentInstaller{}}
	h.svc = NewPowerService(zap.NewNop(), h.installer)
	h.svc.supported = func() bool { return true }
	h.svc.reboot = func() error { h.reboots++; return h.rebootErr }
	h.svc.schedule = func(delay time.Duration, fn func()) {
		h.delays = append(h.delays, delay)
		h.scheduled = append(h.scheduled, fn)
	}
	return h
}

func (h *powerServiceHarness) request(t *testing.T) error {
	t.Helper()
	_, err := h.svc.Reboot(context.Background(), &agentpbv2.RebootRequest{})
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
