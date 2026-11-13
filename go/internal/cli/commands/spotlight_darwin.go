//go:build darwin

package commands

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type preparationCommand func(time.Duration, string, ...string) ([]byte, error)

// stopTargetIndexing only touches mounted FAT slices of the selected disk.
// Failure is advisory: unmounting is mandatory and is attempted independently.
// Never mount a volume merely to stop indexing or disable Spotlight globally.
func stopTargetIndexing(disk string, run preparationCommand) error {
	out, err := run(markerTimeout, "/sbin/mount")
	if err != nil {
		return fmt.Errorf("listing mounted volumes: %w", err)
	}
	var errs []error
	for _, mp := range parseMountedFAT(string(out), disk) {
		out, err := run(markerTimeout, "/usr/bin/sudo", "-n", "/usr/bin/mdutil", "-i", "off", mp)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w (%s)", mp, err, strings.TrimSpace(string(out))))
		}
	}
	return errors.Join(errs...)
}

func parseMountedFAT(out, disk string) []string {
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(disk) + `s[0-9]+ on (.+) \(msdos[,)]`)
	var mounts []string
	for _, line := range strings.Split(out, "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			mounts = append(mounts, m[1])
		}
	}
	return mounts
}
