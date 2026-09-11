// Package meshname defines the private DNS namespace used by Wendy's mesh.
package meshname

import "fmt"

const (
	// Suffix is reserved for Wendy mesh routing. The .internal TLD is reserved
	// for private-use DNS and, unlike .dev, does not force browsers onto HTTPS.
	Suffix = "mesh.wendy.internal"

	// LegacySuffix remains resolvable so deployed apps and older CLIs using the
	// original public/HSTS-preloaded namespace continue to route.
	LegacySuffix = "cloud.wendy.dev"
)

// Device returns the stable private mesh hostname for a cloud asset.
func Device(assetID int32) string {
	return fmt.Sprintf("device-%d.%s", assetID, Suffix)
}
