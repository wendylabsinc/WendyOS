// Package meshname defines the private DNS namespace used by Wendy's mesh.
package meshname

import (
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const (
	// Suffix is reserved for Wendy mesh routing. The .internal TLD is reserved
	// for private-use DNS and, unlike .dev, does not force browsers onto HTTPS.
	Suffix = "wendy.internal"

	// PreviousSuffix is the original private mesh namespace.
	PreviousSuffix = "mesh.wendy.internal"

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
	return fmt.Sprintf("device-%d.%s", assetID, PreviousSuffix)
}

// DeviceFor returns the canonical mesh hostname for a device principal:
// device-<uuid>.wendy.internal, with the canonical lowercase hyphenated
// device UUID.
func DeviceFor(device meshid.Device) string {
	return fmt.Sprintf("device-%s.%s", device.UUID().String(), Suffix)
}

// Suffixes lists current and compatibility namespaces. Match the longest suffix
// first when parsing: PreviousSuffix is itself a child of Suffix.
func Suffixes() []string { return []string{Suffix, PreviousSuffix, LegacySuffix} }

// FriendlyLabel validates a friendly device DNS label. Case is folded,
// but hyphens are preserved so distinct enrolled names remain distinct. The
// identity-based device-<uuid> shape is reserved even if a device uses it as its
// display name.
func FriendlyLabel(name string) (string, bool) {
	// DNS label case folding is ASCII-only. Validate before lowercasing so
	// Unicode compatibility characters cannot create aliases of another device.
	for _, c := range name {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return "", false
		}
	}
	name = strings.ToLower(name)
	if len(name) == 0 || len(name) > 63 || name[0] < 'a' || name[0] > 'z' || name[len(name)-1] == '-' {
		return "", false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return "", false
		}
	}
	if strings.HasPrefix(name, "device-") {
		if _, err := meshid.ParseDeviceUUID(strings.TrimPrefix(name, "device-")); err == nil {
			return "", false
		}
	}
	return name, true
}

// Friendly returns the short hostname for a validated friendly device label.
func Friendly(name string) (string, bool) {
	label, ok := FriendlyLabel(name)
	if !ok {
		return "", false
	}
	return label + "." + Suffix, true
}
