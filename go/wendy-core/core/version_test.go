package core

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestModuleVersion(t *testing.T) {
	for _, test := range []struct {
		name       string
		dependency *debug.Module
		want       string
	}{
		{"tag", &debug.Module{Version: "v0.1.0"}, "0.1.0"},
		{"seed tag", &debug.Module{Version: "v0.1.0-seed.1"}, "0.1.0-seed.1"},
		{"seed pseudo", &debug.Module{Version: "v0.1.0-seed.1.0.20261021200000-a0e8ddc88e76"}, developmentVersion + "+a0e8ddc"},
		{"zero base pseudo", &debug.Module{Version: "v0.0.0-20261021200000-a0e8ddc88e76"}, developmentVersion + "+a0e8ddc"},
		{"prerelease base pseudo", &debug.Module{Version: "v0.1.0-rc.1.0.20261021200000-a0e8ddc88e76"}, developmentVersion + "+a0e8ddc"},
		{"release base pseudo", &debug.Module{Version: "v0.0.2-0.20261021200000-a0e8ddc88e76"}, developmentVersion + "+a0e8ddc"},
		{"incompatible pseudo", &debug.Module{Version: "v2.0.1-0.20261021200000-a0e8ddc88e76+incompatible"}, developmentVersion + "+a0e8ddc"},
		{"prerelease tag", &debug.Module{Version: "v0.1.0-rc.1"}, "0.1.0-rc.1"},
		{"preview tag", &debug.Module{Version: "v0.1.0-pr.3-kb-sdk-2.123.1"}, "0.1.0-pr.3-kb-sdk-2.123.1"},
		{"devel", &debug.Module{Version: "(devel)"}, developmentVersion},
		{"local replacement", &debug.Module{Version: "v0.0.1", Replace: &debug.Module{Path: "../core"}}, developmentVersion},
		{"tagged replacement", &debug.Module{Version: "v0.0.1", Replace: &debug.Module{Version: "v0.2.0"}}, "0.2.0"},
		{"pseudo replacement", &debug.Module{Version: "v0.0.1", Replace: &debug.Module{Version: "v0.0.2-0.20261021200000-a0e8ddc88e76"}}, developmentVersion + "+a0e8ddc"},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.dependency.Path = coreModulePath
			info := &debug.BuildInfo{
				Main:     debug.Module{Path: "consumer"},
				Deps:     []*debug.Module{{Path: "unrelated", Version: "v9.0.0"}, test.dependency},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: strings.Repeat("b", 40)}},
			}
			if got := moduleVersion(info); got != test.want {
				t.Fatalf("Version = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMalformedPseudoVersionsArePreserved(t *testing.T) {
	for _, version := range []string{
		"v0.1.0-20261021200000-a0e8ddc88e76",   // zero-base form requires .0.0
		"v0.1.0-0.20261021200000-a0e8ddc88e76", // release-base form increments the patch
		"v00.0.0-20261021200000-a0e8ddc88e76",
		"v0.01.1-0.20261021200000-a0e8ddc88e76",
		"v0.1.01-0.20261021200000-a0e8ddc88e76",
		"v0.1.0-rc..0.20261021200000-a0e8ddc88e76",
		"v0.1.0-01.0.20261021200000-a0e8ddc88e76",
		"v0.0.0-20261321200000-a0e8ddc88e76",
		"v0.0.0-2026102120000-a0e8ddc88e76",
		"v0.0.0-20261021200000-a0e8ddc88e7",
		"v0.0.0-20261021200000-a0e8ddc88e766",
		"v0.0.0-20261021200000-a0e8ddc88e7g",
		"v0.0.0-20261021200000-A0E8DDC88E76",
		"v0.0.0-20261021200000-a0e8ddc88e76-extra",
	} {
		t.Run(version, func(t *testing.T) {
			if got := sourceVersion(version); got != strings.TrimPrefix(version, "v") {
				t.Fatalf("converted non-pseudo version %q to %q", version, got)
			}
		})
	}
}

func TestMainModuleVersion(t *testing.T) {
	for _, test := range []struct {
		name, path, version, revision, want string
		dirty                               bool
	}{
		{"revision", coreModulePath, "(devel)", strings.Repeat("a", 40), developmentVersion + "+aaaaaaa", false},
		{"dirty revision", coreModulePath, "(devel)", strings.Repeat("a", 40), developmentVersion + "+aaaaaaa", true},
		{"tagged checkout", coreModulePath, "v0.1.0", strings.Repeat("a", 40), developmentVersion + "+aaaaaaa", false},
		{"toolchain dirty pseudo", coreModulePath, "v0.0.2-0.20261021200000-a0e8ddc88e76+dirty", "a0e8ddc88e768f29b07b401046f4caa2cc4dfa4d", developmentVersion + "+a0e8ddc", true},
		{"missing revision", coreModulePath, "(devel)", "", developmentVersion, false},
		{"short revision", coreModulePath, "(devel)", "abcdef", developmentVersion, false},
		{"invalid revision", coreModulePath, "(devel)", "abcdefg", developmentVersion, false},
		{"consumer revision", "consumer", "(devel)", strings.Repeat("b", 40), developmentVersion, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			info := &debug.BuildInfo{Main: debug.Module{Path: test.path, Version: test.version},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: test.revision}}}
			if test.dirty {
				info.Settings = append(info.Settings, debug.BuildSetting{Key: "vcs.modified", Value: "true"})
			}
			if got := moduleVersion(info); got != test.want {
				t.Fatalf("Version = %q, want %q", got, test.want)
			}
		})
	}
	if got := moduleVersion(&debug.BuildInfo{}); got != developmentVersion {
		t.Fatal(got)
	}
}

func TestVersion(t *testing.T) {
	if got := Version(); got != developmentVersion {
		t.Fatalf("source version = %q", got)
	}
}

func TestStampedVersion(t *testing.T) {
	previous := buildVersion
	t.Cleanup(func() { buildVersion = previous })
	buildVersion = "0.1.0-pr.2-kb-sdk-2.123.1"
	if got := Version(); got != buildVersion {
		t.Fatalf("stamped version = %q", got)
	}
}
