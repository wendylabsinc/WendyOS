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
	// reboot asks the host for an orderly reboot and returns only on failure.
	reboot func() error
	// schedule runs fn after delay without blocking the RPC.
	schedule func(delay time.Duration, fn func())

	mu sync.Mutex
	// pending is true from an accepted request until the reboot fails.
	pending bool
}

func NewPowerService(logger *zap.Logger, installer *AgentInstaller) *PowerService {
	return &PowerService{
		logger:    logger,
		installer: installer,
		// The other platforms' rebooter refuses every restart (reboot_other.go).
		supported: func() bool { return runtime.GOOS == "linux" },
		reboot:    defaultReboot,
		schedule:  func(delay time.Duration, fn func()) { time.AfterFunc(delay, fn) },
	}
}

// Reboot answers first and reboots the device shortly afterwards. Who may call
// it is decided by the listener: the setup port of an unenrolled device, an
// operator over mTLS, or the local admin socket. Device credentials are
// refused by the RPC policy before this handler runs.
func (s *PowerService) Reboot(ctx context.Context, _ *agentpbv2.RebootRequest) (*agentpbv2.RebootResponse, error) {
	if !s.supported() {
		return nil, status.Error(codes.Unavailable, "the agent cannot reboot this host")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending {
		// The same reboot, asked for again before the device went down.
		return &agentpbv2.RebootResponse{}, nil
	}
	if !s.installer.TryLock() {
		return nil, status.Error(codes.FailedPrecondition, "an agent or OS update is being installed; reboot the device once it has finished")
	}
	s.pending = true

	caller := "unknown"
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		caller = p.Addr.String()
	}
	s.logger.Warn("Reboot requested; the device restarts shortly",
		zap.String("peer", caller), zap.Duration("delay", rebootRequestDelay))

	s.schedule(rebootRequestDelay, func() {
		// A reboot that takes effect kills this process before it returns.
		err := s.reboot()
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
