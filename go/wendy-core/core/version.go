package core

import (
	"regexp"
	"runtime/debug"
	"strings"
	"time"
)

const developmentVersion = "0.1.0-dev"
const coreModulePath = "github.com/wendylabsinc/WendyOS/go/wendy-core"

// buildVersion is stamped by scripts/build.py for packaged engines.
var buildVersion string

// Version returns the effective Wendy Core version without a leading v.
// Packaged engines report their stamped build identity. Tagged Go dependencies
// retain their version; pseudo-versions use a development version plus seven
// hexadecimal commit characters, for example 0.1.0-dev+a0e8ddc.
// Main-module source builds use VCS revision information when available, without
// a dirty-tree marker. Otherwise, unversioned builds return a development version.
func Version() string {
	if buildVersion != "" {
		return buildVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		return moduleVersion(info)
	}
	return developmentVersion
}

func moduleVersion(info *debug.BuildInfo) string {
	for _, dependency := range info.Deps {
		if dependency.Path != coreModulePath {
			continue
		}
		// A replacement's bytes identify the running engine. Local replacements
		// must not claim the replaced release or the consuming app's VCS revision.
		if dependency.Replace != nil {
			dependency = dependency.Replace
		}
		return sourceVersion(dependency.Version)
	}
	if info.Main.Path == coreModulePath {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && len(setting.Value) >= 7 && isHex(setting.Value) {
				return developmentVersion + "+" + setting.Value[:7]
			}
		}
	}
	return developmentVersion
}

func sourceVersion(version string) string {
	if version == "" || version == "(devel)" {
		return developmentVersion
	}
	if commit := pseudoVersionCommit(version); commit != "" {
		return developmentVersion + "+" + commit[:7]
	}
	return strings.TrimPrefix(version, "v")
}

// Capture the strict numeric base, qualifier, UTC timestamp and 12-digit Git
// revision. The qualifier below distinguishes the three Go pseudo-version forms.
var pseudoVersion = regexp.MustCompile(`^v((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))-(.*?)([0-9]{14})-([0-9a-f]{12})(?:\+incompatible)?$`)
var prereleaseIdentifier = regexp.MustCompile(`^(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)$`)

func pseudoVersionCommit(version string) string {
	parts := pseudoVersion.FindStringSubmatch(version)
	if parts == nil {
		return ""
	}
	if _, err := time.Parse("20060102150405", parts[3]); err != nil {
		return ""
	}
	base, qualifier := parts[1], parts[2]
	switch {
	case qualifier == "": // vX.0.0-timestamp-commit
		if !strings.HasSuffix(base, ".0.0") {
			return ""
		}
	case qualifier == "0.": // vX.Y.(Z+1)-0.timestamp-commit
		if strings.HasSuffix(base, ".0") {
			return ""
		}
	case strings.HasSuffix(qualifier, ".0."): // vX.Y.Z-pre.0.timestamp-commit
		for _, identifier := range strings.Split(strings.TrimSuffix(qualifier, ".0."), ".") {
			if !prereleaseIdentifier.MatchString(identifier) {
				return ""
			}
		}
	default:
		return ""
	}
	return parts[4]
}

func isHex(value string) bool {
	for _, digit := range value {
		if !(digit >= '0' && digit <= '9' || digit >= 'a' && digit <= 'f') {
			return false
		}
	}
	return true
}
