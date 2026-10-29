package commands

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func renderedBuildHostStatus(caps *agentpbv2.GetBuildCapabilitiesResponse, device string) string {
	return ansi.Strip(strings.Join(buildHostStatusLines(caps, device), "\n"))
}

// usbOnlyJetson is the device that motivated the internet check: enabled as a
// build host, connected over USB alone, so its only route is link-local.
func usbOnlyJetson() *agentpbv2.GetBuildCapabilitiesResponse {
	return &agentpbv2.GetBuildCapabilitiesResponse{
		BuilderEnabled:         true,
		Os:                     "linux",
		CpuArchitecture:        "arm64",
		InternetCheckSupported: true,
		InternetReachable:      false,
		InternetDetail:         "cannot resolve registry-1.docker.io",
	}
}

func TestBuildHostStatusLines_InternetNotReachable(t *testing.T) {
	got := renderedBuildHostStatus(usbOnlyJetson(), "mesh-1.local")
	for _, want := range []string{
		"Builder role: enabled",
		"BuildKit:     unavailable (install buildkitd there and start it on unix:///run/buildkit/buildkitd.sock)",
		"Internet:     not reachable (cannot resolve registry-1.docker.io)",
		"`wendy device wifi connect --device mesh-1.local`, or plug in ethernet",
		"Platform:     linux/arm64",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q; got:\n%s", want, got)
		}
	}
}

func TestBuildHostStatusLines_InternetReachable(t *testing.T) {
	caps := &agentpbv2.GetBuildCapabilitiesResponse{
		BuilderEnabled:         true,
		BuildkitAvailable:      true,
		BuildkitVersion:        "v0.16.0",
		Os:                     "linux",
		CpuArchitecture:        "arm64",
		NativePlatforms:        []string{"linux/arm64"},
		InternetCheckSupported: true,
		InternetReachable:      true,
	}
	got := renderedBuildHostStatus(caps, "spark-3.local")
	if !strings.Contains(got, "Internet:     reachable") {
		t.Errorf("status lacks the reachable line; got:\n%s", got)
	}
	if strings.Contains(got, "wifi connect") || strings.Contains(got, "install buildkitd") {
		t.Errorf("a working host must not be told to fix anything; got:\n%s", got)
	}
}

// An older agent sends no internet fields, so its zero internet_reachable is
// "did not check". Rendering it as "not reachable" would send people to fix a
// network that works.
func TestBuildHostStatusLines_OlderAgentSaysNothingAboutInternet(t *testing.T) {
	caps := usbOnlyJetson()
	caps.InternetCheckSupported, caps.InternetDetail = false, ""
	got := renderedBuildHostStatus(caps, "mesh-1.local")
	if strings.Contains(got, "Internet") || strings.Contains(got, "wifi connect") {
		t.Errorf("an agent without the check must get no internet line; got:\n%s", got)
	}
}

// The remedy is pasted into a shell, so a cloud display name, which is not a
// selector, must become a placeholder rather than a command selecting nothing.
func TestBuildHostInternetRemedy_PlaceholderForNonSelectors(t *testing.T) {
	for device, want := range map[string]string{
		"mesh-1.local":          "--device mesh-1.local`",
		"":                      "--device <device>`",
		"Spark 3 [asset 472]":   "--device <device>`",
		"Unitree G1 Nx":         "--device <device>`",
		"10.10.20.107:50052":    "--device 10.10.20.107:50052`",
		"jetson-orin-nano-a1b2": "--device jetson-orin-nano-a1b2`",
	} {
		if got := buildHostInternetRemedy(device); !strings.Contains(got, want) {
			t.Errorf("buildHostInternetRemedy(%q) = %q, want it to contain %q", device, got, want)
		}
	}
}

func TestWriteBuildCapabilitiesJSON_IncludesInternetFields(t *testing.T) {
	var out bytes.Buffer
	if err := writeBuildCapabilitiesJSON(&out, usbOnlyJetson()); err != nil {
		t.Fatalf("writeBuildCapabilitiesJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	// internet_reachable is false, and must still be present: a script reading
	// this should not have to know proto3 drops zero values.
	for key, want := range map[string]any{
		"internet_check_supported": true,
		"internet_reachable":       false,
		"internet_detail":          "cannot resolve registry-1.docker.io",
		"builder_enabled":          true,
		"buildkit_available":       false,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
}

func TestBuildHostInternetNotice(t *testing.T) {
	t.Run("not reachable", func(t *testing.T) {
		got := buildHostInternetNotice("mesh-1.local", usbOnlyJetson())
		for _, want := range []string{
			"mesh-1.local reports no internet access (cannot resolve registry-1.docker.io)",
			"cannot pull base images",
			"`wendy device wifi connect --device mesh-1.local`, or plug in ethernet",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("notice lacks %q; got %q", want, got)
			}
		}
	})
	t.Run("reachable", func(t *testing.T) {
		caps := usbOnlyJetson()
		caps.InternetReachable, caps.InternetDetail = true, ""
		if got := buildHostInternetNotice("spark-3.local", caps); got != "" {
			t.Errorf("want no notice for a reachable host, got %q", got)
		}
	})
	t.Run("older agent", func(t *testing.T) {
		caps := usbOnlyJetson()
		caps.InternetCheckSupported = false
		if got := buildHostInternetNotice("spark-3.local", caps); got != "" {
			t.Errorf("want no notice from an agent that does not check, got %q", got)
		}
	})
}

// The run preflight warns and carries on: a build with every layer cached, or
// base images from a local registry, can still succeed without internet.
func TestCheckBuildHostCapabilities_WarnsButAllowsAHostWithNoInternet(t *testing.T) {
	caps := usbOnlyJetson()
	caps.BuildkitAvailable = true
	caps.NativePlatforms = []string{"linux/arm64"}
	var err error
	stderr := captureStderr(t, func() {
		err = checkBuildHostCapabilities("mesh-1.local", caps, "linux/arm64")
	})
	if err != nil {
		t.Fatalf("no internet must warn, not refuse; got %v", err)
	}
	if !strings.Contains(stderr, "mesh-1.local reports no internet access") {
		t.Errorf("want the internet notice before the build; stderr:\n%s", stderr)
	}
}

// With no BuildKit as well, the refusal still happens, and the notice comes
// first: it is why the developer cannot install BuildKit there either.
func TestCheckBuildHostCapabilities_NoticesNoInternetBeforeRefusingNoBuildkit(t *testing.T) {
	var err error
	stderr := captureStderr(t, func() {
		err = checkBuildHostCapabilities("mesh-1.local", usbOnlyJetson(), "linux/arm64")
	})
	if err == nil || !strings.Contains(err.Error(), "no BuildKit daemon") {
		t.Fatalf("a host with no BuildKit must still be refused; got %v", err)
	}
	if !strings.Contains(stderr, "no internet access") {
		t.Errorf("want the internet notice alongside the refusal; stderr:\n%s", stderr)
	}
}

func TestCheckBuildHostCapabilities_SilentAboutInternetForOlderAgent(t *testing.T) {
	stderr := captureStderr(t, func() {
		if err := checkBuildHostCapabilities("spark-office", &agentpbv2.GetBuildCapabilitiesResponse{
			BuilderEnabled:    true,
			BuildkitAvailable: true,
			NativePlatforms:   []string{"linux/arm64"},
		}, "linux/arm64"); err != nil {
			t.Fatalf("checkBuildHostCapabilities: %v", err)
		}
	})
	if strings.Contains(stderr, "internet") {
		t.Errorf("an agent that does not check must not produce an internet notice; stderr:\n%s", stderr)
	}
}
