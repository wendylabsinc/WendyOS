package commands

import (
	"maps"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/buildargs"
)

// mergeBuildArgFlags applies explicit CLI arguments after device hints and
// Compose args, before hashing or building. Repeated keys use the last value.
func mergeBuildArgFlags(defaults map[string]string, entries []string) (map[string]string, error) {
	merged := make(map[string]string, len(defaults)+len(entries))
	maps.Copy(merged, defaults)
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, usageErrorf("--build-arg requires KEY=VALUE")
		}
		if err := buildargs.ValidatePair(key, value); err != nil {
			return nil, usageErrorf("--build-arg: %s", err)
		}
		merged[key] = value
	}
	return merged, nil
}
