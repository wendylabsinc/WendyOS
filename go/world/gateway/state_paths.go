package gateway

import (
	"errors"
	"os"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

// Validate every owned namespace before opening any writer. Store sidecars and
// the replay staging directory are part of ownership, even before they exist.
func validateStatePaths(paths []string, configPath, replayPath string) error {
	groups := make([][]string, 0, len(paths)+1)
	for _, path := range paths {
		owned := []string{path, path + ".lock"}
		if path == replayPath {
			owned = append(owned, path+authority.ReplayStagingSuffix)
		}
		groups = append(groups, owned)
	}
	groups = append(groups, []string{configPath})
	var previous []string
	var existing []os.FileInfo
	for _, group := range groups {
		for _, path := range group {
			resolved, err := canonicalDiagnosticPath(path, 0)
			if err != nil {
				return err
			}
			for _, other := range previous {
				if pathWithin(resolved, other) || pathWithin(other, resolved) {
					return errStatePaths
				}
			}
			info, err := os.Stat(resolved)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil {
				for _, other := range existing {
					if os.SameFile(info, other) {
						return errStatePaths
					}
				}
				existing = append(existing, info)
			}
			previous = append(previous, resolved)
		}
	}
	return nil
}
