package babel

import (
	"net"
)

// Router ID derivation for embedding applications.
//
// A router ID is routing state, not a certificate identity and not a Cloud
// field: there is no centralized router-ID reservation and no signed manifest
// binding a router ID to a device identity. Generate one ID for the active
// engine instance and share it across all of that instance's links.
//
// The engine already refuses an entirely zero or entirely one router ID in
// New and in checkpoint Restore; Restore additionally refuses state that
// belongs to a different router ID. If router IDs change between runs,
// initialize or restore the engine and its checkpoints consistently.

// RouterIDFromMAC composes the permanent form 0x0000 || MAC48 from a selected
// factory/permanent hardware address. Use the permanent address, not whichever
// randomized network address is currently advertised: the router ID must not
// change when the active carrier changes. An all-zero purported MAC is treated
// as unavailable. All-ones MAC bits under the 0x0000 tag cannot produce the
// engine's forbidden all-ones router ID.
func RouterIDFromMAC(mac net.HardwareAddr) (RouterID, error) {
	if len(mac) != 6 {
		return 0, ErrConfig
	}
	var id RouterID
	for _, b := range mac {
		id = id<<8 | RouterID(b)
	}
	if id == 0 {
		return 0, ErrConfig
	}
	return id, nil
}

// RouterIDFromRandom composes the fallback 0x0001 || RANDOM48 from
// caller-supplied entropy; the engine itself consults no entropy. The 0x0001
// tag keeps the result away from both engine-prohibited values for any random
// input. The embedding application must keep the result stable for the router
// instance's lifetime.
func RouterIDFromRandom(random [6]byte) RouterID {
	var r RouterID
	for _, b := range random {
		r = r<<8 | RouterID(b)
	}
	return RouterID(1)<<48 | r
}
