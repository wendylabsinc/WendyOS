package commands

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// formatBytes converts a byte count to a human-readable string using SI units
// (powers of 1000: kB, MB, GB). This is the package-level helper used by
// both the apps dashboard and volumes commands.
func formatBytes(n int64) string {
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

func formatDiskUsage(usedBytes, totalBytes int64) string {
	return fmt.Sprintf("%s / %s", formatGigabytes(usedBytes), formatGigabytes(totalBytes))
}

func formatGigabytes(n int64) string {
	s := fmt.Sprintf("%.2f", float64(n)/1_000_000_000)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + " GB"
}

// formatPartitionTable renders an aligned, human-readable table of per-partition
// disk usage suitable for printing under the device info output. The returned
// string ends with a trailing newline.
func formatPartitionTable(partitions []*agentpb.DiskPartition, storage ...*agentpb.DiskPartition) string {
	var b strings.Builder
	b.WriteString("Disk Usage:\n")
	var containerStorage *agentpb.DiskPartition
	if len(storage) > 0 && storage[0] != nil {
		containerStorage = storage[0]
		ordered := []*agentpb.DiskPartition{containerStorage}
		for _, p := range partitions {
			if !samePartition(p, containerStorage) {
				ordered = append(ordered, p)
			}
		}
		partitions = ordered
	}

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  MOUNTPOINT\tFILESYSTEM\tUSED\tTOTAL\tUSE%")
	for _, p := range partitions {
		name := p.GetMountpoint()
		if samePartition(p, containerStorage) {
			name += " (container storage)"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			name,
			p.GetFilesystem(),
			formatGigabytes(p.GetUsedBytes()),
			formatGigabytes(p.GetTotalBytes()),
			formatUsePercent(p.GetUsedBytes(), p.GetTotalBytes()),
		)
	}
	tw.Flush()

	return b.String()
}

// formatUsePercent returns the used fraction as a whole-number percent string
// (e.g. "16%"), or "-" when the total is unknown.
func formatUsePercent(used, total int64) string {
	if total <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", used*100/total)
}
