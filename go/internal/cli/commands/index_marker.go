//go:build darwin

package commands

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Keep markers for older images without baked-in protection. Spotlight
// interference with the macOS FAT driver is suspected, not confirmed.
const spotlightMarker = ".metadata_never_index"

const markerTimeout = 10 * time.Second

// Each probe, mount, marker write and unmount is a bounded subprocess. Do not
// abandon a goroutine that can later mount or modify the card after eject.
func markFATVolumes(d drive) {
	if err := markFATVolumesUnindexed(d); err != nil {
		fmt.Printf("Note: could not mark %s's FAT volumes to skip Spotlight indexing: %v\n", d.DevicePath, err)
	}
}

func parseDiskutilSlices(out, diskDev string) []string {
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(strings.TrimPrefix(diskDev, "/dev/")) + `s[0-9]+$`)
	var slices []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && re.MatchString(fields[len(fields)-1]) {
			slices = append(slices, "/dev/"+fields[len(fields)-1])
		}
	}
	return slices
}

func parseBundleType(info string) string {
	return diskInfoField(info, "Type (Bundle):")
}

func diskInfoField(info, key string) string {
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
