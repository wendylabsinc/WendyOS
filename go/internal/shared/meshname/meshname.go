// Package meshname defines the private DNS namespace used by Wendy's mesh.
package meshname

import (
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const (
	// Suffix is reserved for Wendy mesh routing. The .internal TLD is reserved
	// for private-use DNS and, unlike .dev, does not force browsers onto HTTPS.
	Suffix = "mesh.wendy.internal"

	// LegacySuffix remains resolvable so deployed apps and older CLIs using the
	// original public/HSTS-preloaded namespace continue to route.
	LegacySuffix = "cloud.wendy.dev"
)

// Device returns the stable private mesh hostname for a cloud asset.
//
// Legacy v1 asset form: it keys the name on a Cloud asset ID, which is a
// database record ID and not a mesh identity. Only the CLI cloud-tunnel path
// for pre-v2 assets still needs it; new callers must use DeviceFor.
func Device(assetID int32) string {
	return fmt.Sprintf("device-%d.%s", assetID, Suffix)
}

// DeviceFor returns the canonical mesh hostname for a device principal:
// device-<uuid>.mesh.wendy.internal, with the canonical lowercase hyphenated
// device UUID.
func DeviceFor(device meshid.Device) string {
	return fmt.Sprintf("device-%s.%s", device.UUID().String(), Suffix)
}
