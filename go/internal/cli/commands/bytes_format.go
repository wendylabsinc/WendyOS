package commands

import "fmt"

// formatBytes converts a byte count to a human-readable string using SI units
// (powers of 1000: kB, MB, GB). This is the package-level helper used by
// both the apps dashboard and volumes commands.
func formatBytes(n int64) string {
	if n < 0 {
		return fmt.Sprintf("%d B", n)
	}
	return formatBytesUint(uint64(n))
}

func formatBytesUint(n uint64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1f GB", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1f MB", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1f kB", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
