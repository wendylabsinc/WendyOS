//go:build linux

package meshsharing

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
)

// CleanupStale withdraws Internet-sharing state left by a prior agent without
// loading credentials or starting a sharing policy. Call it at startup before
// creating a Controller, or after the live Controller has been closed.
func CleanupStale(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Withdraw routes before their firewall guards. An ownership conflict or
	// failed deletion keeps the remaining guards in place for a later retry.
	if err := reclaimInternetTunnels(); err != nil {
		return fmt.Errorf("reclaim Wendy Net Internet tunnels: %w", err)
	}
	if err := reclaimDonorRouting(); err != nil {
		return fmt.Errorf("reclaim Wendy Net Internet donor routing: %w", err)
	}
	// Restore a crashed gateway's owned baseline before removing its guard.
	// Both forwarding helpers retain other live consumers and foreign state.
	if err := hostnetwork.ReconcileIPv6Forwarding(); err != nil {
		return fmt.Errorf("restore Wendy Net Internet IPv6 forwarding: %w", err)
	}
	if err := reclaimInternetRules(ctx); err != nil {
		return fmt.Errorf("reclaim Wendy Net Internet firewall rules: %w", err)
	}
	if err := hostnetwork.ReleaseForwarding("mesh-internet"); err != nil {
		return fmt.Errorf("release Wendy Net Internet IPv4 forwarding: %w", err)
	}
	// Resolver restoration is handled by live Controller.Close and by
	// localmesh.CleanupStaleResolver's durable ownership journal at startup.
	// Removing the owned mesh links also discards their resolved DNS settings.
	// Do not flush shared address pools here: unrelated conntrack entries have
	// no ownership marker. The next lease allocator checks conflicts first.
	return nil
}
