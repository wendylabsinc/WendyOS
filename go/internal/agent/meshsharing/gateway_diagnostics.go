package meshsharing

import (
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
)

// Describe the last selected donor without changing eligibility. These inputs
// are already authenticated directory state; diagnostic dates never extend it.
func gatewayEvidence(peer meshid.Device, view localmesh.NodeSnapshot, offers []meshcatalog.Record) []zap.Field {
	var manifestExpiry, offerExpiry int64
	var manifestWithdrawn, offerWithdrawn, routed bool
	for _, manifest := range view.Devices {
		if manifest.Device == peer.UUID() && manifest.Expires >= manifestExpiry {
			manifestExpiry, manifestWithdrawn = manifest.Expires, manifest.Withdraw
		}
	}
	for _, offer := range offers {
		if meshcatalog.IsGatewayOffer(offer) && offer.Key.Device == peer.Principal() && offer.Expires >= offerExpiry {
			offerExpiry, offerWithdrawn = offer.Expires, offer.Withdraw
		}
	}
	for _, route := range view.Routes {
		if route.Prefix.Addr() == peer.MeshAddress() && route.Prefix.Bits() == 128 && !route.Local && !route.Unreachable && route.Link != 0 {
			routed = true
		}
	}
	return []zap.Field{zap.Bool("route_present", routed),
		zap.Int64("manifest_expires_unix_ms", manifestExpiry), zap.Bool("manifest_withdrawn", manifestWithdrawn),
		zap.Int64("offer_expires_unix_ms", offerExpiry), zap.Bool("offer_withdrawn", offerWithdrawn), zap.Time("observed_at", time.Now())}
}
