package meshview

import (
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// AnnouncementRouteDeadline joins public fields from ONE authenticated local
// topology observation. OriginAuthorized describes this exact independently
// verified route tuple, not authority borrowed from the current announcement.
// During a pure sequence handoff Directory may retain one older tuple; its own
// deadline bounds consumers of the unchanged current endpoint/service metadata.
// The caller still checks the reporter, target identity and observation age.
func AnnouncementRouteDeadline(route *pb.LocalMeshRoute, hint *pb.LocalMeshAnnouncement, now time.Time) (int64, bool) {
	if route == nil || hint == nil || hint.Principal == "" || hint.Prefix == "" || hint.TrustState != "verified-at-snapshot" ||
		hint.ExpiresAtUnixMs <= now.UnixMilli() || route.SignedRouteExpiresAtUnixMs <= now.UnixMilli() ||
		!route.OriginAuthorized || route.Unreachable || route.Metric >= uint32(babel.Infinity) ||
		route.OriginPrincipal != hint.Principal || route.Prefix != hint.Prefix || route.OriginRouterId == 0 || route.OriginRouterId != hint.OriginRouterId ||
		route.SequenceNumber > 65535 || hint.SequenceNumber > 65535 {
		return 0, false
	}
	if route.SequenceNumber == hint.SequenceNumber {
		// Preserve the exact current-tuple binding used by legacy projections.
		return route.SignedRouteExpiresAtUnixMs, route.SignedRouteExpiresAtUnixMs == hint.ExpiresAtUnixMs
	}
	advance := uint16(hint.SequenceNumber) - uint16(route.SequenceNumber)
	if advance >= 1<<15 || route.SignedRouteExpiresAtUnixMs > hint.ExpiresAtUnixMs {
		return 0, false
	}
	return route.SignedRouteExpiresAtUnixMs, true
}
