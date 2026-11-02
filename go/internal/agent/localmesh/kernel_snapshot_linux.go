//go:build linux

package localmesh

import (
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"
)

const kernelSnapshotAttempts = 5

// Interface/address changes can interrupt an otherwise successful netlink dump.
// Its partial result is not an authoritative ownership snapshot. Restart the
// whole read, including link identity, before ReconcileBase makes any changes.
// Persistent churn and every other error retain the existing failure behavior.
func readConsistentBase(read func() (baseState, error)) (baseState, error) {
	var last error
	for range kernelSnapshotAttempts {
		state, err := read()
		if err == nil {
			return state, nil
		}
		if !errors.Is(err, netlink.ErrDumpInterrupted) {
			return baseState{}, err
		}
		last = err
	}
	return baseState{}, fmt.Errorf("wendy net snapshot interrupted after %d attempts: %w", kernelSnapshotAttempts, last)
}
