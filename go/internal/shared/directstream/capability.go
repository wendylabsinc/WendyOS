package directstream

// Profile 2 requires recovery, epoch retirement, live lane reuse and datagram
// capacity records. Only the promoted transport is negotiated. Profile 1 was
// developmental; it falls back to ordinary upper connections.
const (
	CapabilityVersion uint32 = 2
	CapabilityTLS     uint32 = 1 << 8
	CapabilityQUIC    uint32 = 1 << 9
)

func ValidCapability(c uint32) bool {
	return c == 0 || c&255 == CapabilityVersion && c & ^uint32(255|CapabilityTLS|CapabilityQUIC) == 0 && c&(CapabilityTLS|CapabilityQUIC) != 0
}
func NegotiatedCapability(local, remote uint32) uint32 {
	if !ValidCapability(local) || local == 0 || !ValidCapability(remote) || remote == 0 {
		return 0
	}
	common := local & remote
	if common&CapabilityQUIC != 0 {
		return CapabilityVersion | CapabilityQUIC
	}
	if common&CapabilityTLS != 0 {
		return CapabilityVersion | CapabilityTLS
	}
	return 0
}
func SelectedTransport(c uint32) uint32 {
	if c == CapabilityVersion|CapabilityTLS || c == CapabilityVersion|CapabilityQUIC {
		return c
	}
	return 0
}

// A busy peer parks the lane until its existing application epoch has drained.
func NegotiatedCarrier(local, localDeferred, remote, remoteDeferred uint32) (uint32, bool) {
	if selected := NegotiatedCapability(local, remote); selected != 0 {
		return selected, false
	}
	if local == 0 {
		local = localDeferred
	}
	if remote == 0 {
		remote = remoteDeferred
	}
	selected := NegotiatedCapability(local, remote)
	return selected, selected != 0
}
