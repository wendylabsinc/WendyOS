package version

import (
	"strconv"
	"strings"
)

// Version is set at build time via -ldflags
var Version = "dev"

// CompareVersions returns -1 if a < b, 0 if a == b, +1 if a > b.
// Handles semver (e.g. "0.10.0"), date-based (e.g. "v2025.06.02-133859"),
// and mixed formats by splitting on ".", "-" and comparing each component
// numerically when possible, falling back to lexicographic comparison.
// "dev" is treated as always less than any real version.
func CompareVersions(a, b string) int {
	if a == b {
		return 0
	}
	if a == "dev" {
		return -1
	}
	if b == "dev" {
		return 1
	}

	aParts := splitVersion(strings.TrimPrefix(a, "v"))
	bParts := splitVersion(strings.TrimPrefix(b, "v"))

	n := len(aParts)
	if len(bParts) > n {
		n = len(bParts)
	}

	for i := 0; i < n; i++ {
		var ap, bp string
		if i < len(aParts) {
			ap = aParts[i]
		}
		if i < len(bParts) {
			bp = bParts[i]
		}

		// Try numeric comparison first.
		aNum, aErr := strconv.Atoi(ap)
		bNum, bErr := strconv.Atoi(bp)
		if aErr == nil && bErr == nil {
			if aNum < bNum {
				return -1
			}
			if aNum > bNum {
				return 1
			}
			continue
		}

		// Fall back to lexicographic comparison.
		if ap < bp {
			return -1
		}
		if ap > bp {
			return 1
		}
	}

	return 0
}

// splitVersion splits a version string on "." and "-" delimiters,
// preserving the order of components.
func splitVersion(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool {
		return r == '.' || r == '-'
	})
}
