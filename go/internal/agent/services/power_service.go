package services

import (
	"context"
	"runtime"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// rebootRequestDelay lets the Reboot response reach the caller before the
// device goes down. It matches the delay a power mode switch uses.
const rebootRequestDelay = 2 * time.Second

// forcedRecoveryRebootArgument is the reboot(2) argument that boots a Jetson
// into USB forced-recovery mode: NVIDIA's Tegra PMC driver sets the boot ROM's
// recovery flag when a restart carries it, as `reboot forced-recovery` does.
// The driver's "recovery" argument is a different mode and is not this one.
const forcedRecoveryRebootArgument = "forced-recovery"

// PowerService reboots the device on request. It is the remote recovery for a
// device that is reachable but stuck: before it, the only reboots the agent
// performed were side effects of an OS update or a Jetson power mode switch.
type PowerService struct {
	agentpbv2.UnimplementedWendyPowerServiceServer
	logger *zap.Logger
	// installer is the lock agent and OS updates hold while they write. Reboot
	// takes it too: a reboot must not interrupt an install, and no install may
	// start once the device is on its way down.
	installer *AgentInstaller

	// supported reports whether the agent can reboot this host at all.
	supported func() bool
	// hasForcedRecovery reports whether the board has a forced-recovery mode.
	hasForcedRecovery func() bool
	// reboot asks the host for an orderly reboot into mode and returns only
	// on failure.
	reboot func(mode agentpbv2.RebootMode) error
	// schedule runs fn after delay without blocking the RPC.
	schedule func(delay time.Duration, fn func())

	mu sync.Mutex
	// pending is true from an accepted request until the reboot fails.
	pending bool
	// pendingMode is the mode of the pending reboot.
	pendingMode agentpbv2.RebootMode
}

func NewPowerService(logger *zap.Logger, installer *AgentInstaller) *PowerService {
	return &PowerService{
		logger:    logger,
		installer: installer,
		// The other platforms' rebooter refuses every restart (reboot_other.go).
		supported:         func() bool { return runtime.GOOS == "linux" },
		hasForcedRecovery: defaultIsJetson,
		reboot:            defaultRebootInto,
		schedule:          func(delay time.Duration, fn func()) { time.AfterFunc(delay, fn) },
	}
}

// defaultRebootInto takes the same orderly path for every mode; only the
// argument the restart hands to the kernel differs.
func defaultRebootInto(mode agentpbv2.RebootMode) error {
	if mode == agentpbv2.RebootMode_REBOOT_MODE_FORCED_RECOVERY {
		return newArgumentRebooter(nil, forcedRecoveryRebootArgument).rebootClean()
	}
	return defaultReboot()
}

// describeRebootMode names a reboot in messages.
func describeRebootMode(mode agentpbv2.RebootMode) string {
	if mode == agentpbv2.RebootMode_REBOOT_MODE_FORCED_RECOVERY {
		return "reboot into forced-recovery mode"
	}
	return "normal reboot"
}

// Reboot answers first and reboots the device shortly afterwards. Who may call
// it is decided by the listener: the setup port of an unenrolled device, an
// operator over mTLS, or the local admin socket. Device credentials are
// refused by the RPC policy before this handler runs.
//
// A request for a special boot is refused rather than carried out as a normal
// reboot, which would leave the caller believing the device is somewhere it
// is not.
func (s *PowerService) Reboot(ctx context.Context, req *agentpbv2.RebootRequest) (*agentpbv2.RebootResponse, error) {
	mode := req.GetMode()
	switch mode {
	case agentpbv2.RebootMode_REBOOT_MODE_UNSPECIFIED, agentpbv2.RebootMode_REBOOT_MODE_FORCED_RECOVERY:
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown reboot mode %d; update the agent with 'wendy device update' to use it", mode)
	}
	if !s.supported() {
		return nil, status.Error(codes.Unavailable, "the agent cannot reboot this host")
	}
	if mode == agentpbv2.RebootMode_REBOOT_MODE_FORCED_RECOVERY && !s.hasForcedRecovery() {
		return nil, status.Error(codes.FailedPrecondition, "forced-recovery mode exists only on NVIDIA Jetson boards, and this device is not one")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending {
		if mode == s.pendingMode {
			// The same reboot, asked for again before the device went down.
			return &agentpbv2.RebootResponse{}, nil
		}
		return nil, status.Errorf(codes.FailedPrecondition, "a %s is already under way", describeRebootMode(s.pendingMode))
	}
	if !s.installer.TryLock() {
		return nil, status.Error(codes.FailedPrecondition, "an agent or OS update is being installed; reboot the device once it has finished")
	}
	s.pending = true
	s.pendingMode = mode

	caller := "unknown"
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		caller = p.Addr.String()
	}
	s.logger.Warn("Reboot requested; the device restarts shortly",
		zap.String("peer", caller), zap.String("mode", describeRebootMode(mode)),
		zap.Duration("delay", rebootRequestDelay))

	s.schedule(rebootRequestDelay, func() {
		// A reboot that takes effect kills this process before it returns.
		err := s.reboot(mode)
		if err == nil {
			return
		}
		// The device stays up, so it must stay usable: let updates in again
		// and let a later request try again.
		s.logger.Error("Requested reboot did not happen; the device stays up", zap.Error(err))
		s.mu.Lock()
		s.pending = false
		s.mu.Unlock()
		s.installer.Unlock()
	})
	return &agentpbv2.RebootResponse{}, nil
}
