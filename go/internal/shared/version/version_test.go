package version

import "testing"

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		// Equal.
		{"1.0.0", "1.0.0", 0},
		{"dev", "dev", 0},

		// Dev is always less.
		{"dev", "0.1.0", -1},
		{"0.1.0", "dev", 1},

		// Basic semver.
		{"0.9.3", "0.9.8", -1},
		{"0.9.8", "0.9.3", 1},
		{"0.7.0", "0.9.3", -1},

		// Multi-digit components (the key bug).
		{"0.9.8", "0.10.0", -1},
		{"0.10.0", "0.9.8", 1},
		{"0.10.1", "0.10.2", -1},
		{"1.0.0", "0.99.99", 1},

		// Date-based versions.
		{"2025.06.02-133859", "2025.06.02-140000", -1},
		{"2025.06.03-100000", "2025.06.02-235959", 1},

		// With v prefix.
		{"v0.10.0", "v0.9.8", 1},
		{"v1.0.0", "0.99.0", 1},

		// Different lengths.
		{"1.0", "1.0.0", -1}, // "1.0" has fewer parts, missing part treated as ""
		{"1.0.0", "1.0", 1},
	}

	for _, tt := range tests {
		got := CompareVersions(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
