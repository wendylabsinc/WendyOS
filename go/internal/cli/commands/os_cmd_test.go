package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestOSAlreadyCurrent(t *testing.T) {
	tests := []struct {
		name    string
		current string
		latest  string
		nightly bool
		want    bool
	}{
		{"stable equal is current", "WendyOS-0.10.4", "0.10.4", false, true},
		{"stable newer available", "WendyOS-0.10.4", "0.12.0", false, false},
		{"stable device ahead is current", "WendyOS-0.12.0", "0.10.4", false, true},
		{"nightly equal is current", "WendyOS-0.12.0-nightly", "0.12.0-nightly", true, true},
		{"nightly different available", "WendyOS-0.12.0-nightly", "0.13.0-nightly", true, false},
		{"empty current not current", "", "0.10.4", false, false},
		{"empty latest not current", "WendyOS-0.10.4", "", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := osAlreadyCurrent(tc.current, tc.latest, tc.nightly); got != tc.want {
				t.Fatalf("osAlreadyCurrent(%q,%q,%v) = %v, want %v", tc.current, tc.latest, tc.nightly, got, tc.want)
			}
		})
	}
}

func TestRequireReflashableOSVersion(t *testing.T) {
	tests := []struct {
		name      string
		osVersion string
		wantErr   bool
	}{
		{"pre-0.17 blocked", "WendyOS-0.16.0", true},
		{"pre-0.17 nightly blocked", "WendyOS-0.16.0-nightly", true},
		{"pre-0.17 without prefix blocked", "0.16.0", true},
		{"much older blocked", "WendyOS-0.10.4", true},
		{"two-component older blocked", "WendyOS-0.16", true},
		{"exactly 0.17.0 allowed", "WendyOS-0.17.0", false},
		{"0.17.0 nightly allowed", "WendyOS-0.17.0-nightly", false},
		{"patch newer allowed", "WendyOS-0.17.1", false},
		{"minor newer allowed", "WendyOS-0.18.0", false},
		{"dev allowed", "dev", false},
		{"dev suffix allowed", "2026.06.30-133859-dev", false},
		{"empty allowed", "", false},
		{"unparseable allowed", "garbage", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := requireReflashableOSVersion(tc.osVersion)
			if tc.wantErr != (err != nil) {
				t.Fatalf("requireReflashableOSVersion(%q) err = %v, wantErr = %v", tc.osVersion, err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "wendy os install") {
				t.Errorf("error message should point to `wendy os install`, got: %v", err)
			}
		})
	}
}

// TestOSUpdateShouldSkipAlreadyCurrent pins the fix for the `os update --pr N`
// re-flash bug: a PR's resolved version tag ("pr-N") is constant across
// rebuilds, so before this fix a second `update --pr N` after pushing a new
// commit to the same PR would compare "pr-N" == "pr-N" and silently no-op with
// "already at latest" instead of re-flashing. The non-PR path (prNumber == 0)
// must keep deferring entirely to osAlreadyCurrent.
func TestOSUpdateShouldSkipAlreadyCurrent(t *testing.T) {
	tests := []struct {
		name     string
		prNumber int
		current  string
		latest   string
		nightly  bool
		want     bool
	}{
		{"non-PR already current still short-circuits", 0, "WendyOS-0.10.4", "0.10.4", false, true},
		{"non-PR newer available does not short-circuit", 0, "WendyOS-0.10.4", "0.12.0", false, false},
		{"PR re-test with identical pr-N tag never short-circuits", 123, "WendyOS-pr-123", "pr-123", false, false},
		{"PR first-time install never short-circuits even when versions differ", 123, "WendyOS-0.10.4", "pr-123", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := osUpdateShouldSkipAlreadyCurrent(tc.prNumber, tc.current, tc.latest, tc.nightly); got != tc.want {
				t.Fatalf("osUpdateShouldSkipAlreadyCurrent(%d,%q,%q,%v) = %v, want %v",
					tc.prNumber, tc.current, tc.latest, tc.nightly, got, tc.want)
			}
		})
	}
}

func TestDecideOSUpdate(t *testing.T) {
	tests := []struct {
		name        string
		prNumber    int
		current     string
		latest      string
		nightly     bool
		assumeYes   bool
		interactive bool
		want        osUpdateAction
	}{
		{"already current", 0, "WendyOS-0.10.4", "0.10.4", false, false, false, osActionAlreadyCurrent},
		{"newer with yes", 0, "WendyOS-0.10.4", "0.12.0", false, true, false, osActionApply},
		{"newer with yes overrides tty", 0, "WendyOS-0.10.4", "0.12.0", false, true, true, osActionApply},
		{"newer interactive prompts", 0, "WendyOS-0.10.4", "0.12.0", false, false, true, osActionPrompt},
		{"newer noninteractive reports", 0, "WendyOS-0.10.4", "0.12.0", false, false, false, osActionReportOnly},
		{"pr identical tag interactive prompts", 123, "WendyOS-pr-123", "pr-123", false, false, true, osActionPrompt},
		{"pr identical tag with yes applies", 123, "WendyOS-pr-123", "pr-123", false, true, false, osActionApply},
		{"pr noninteractive reports", 123, "WendyOS-0.17.0", "pr-123", false, false, false, osActionReportOnly},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideOSUpdate(tc.prNumber, tc.current, tc.latest, tc.nightly, tc.assumeYes, tc.interactive)
			if got != tc.want {
				t.Fatalf("decideOSUpdate(pr=%d,%q,%q,nightly=%v,yes=%v,tty=%v) = %v, want %v",
					tc.prNumber, tc.current, tc.latest, tc.nightly, tc.assumeYes, tc.interactive, got, tc.want)
			}
		})
	}
}

func TestValidateOSUpdateIdentityAllowsWendyOSBeforeBackendCheck(t *testing.T) {
	osVersion := "WendyOS-0.10.4"
	cases := []*agentpb.GetAgentVersionResponse{
		{Os: "linux", OsVersion: &osVersion},
		{Os: "linux", OsVersion: &osVersion, Featureset: []string{"wendyos-update"}},
	}
	for _, resp := range cases {
		if err := validateOSUpdateIdentity(resp); err != nil {
			t.Fatalf("validateOSUpdateIdentity(%+v) error = %v, want nil", resp, err)
		}
	}
}

// Since #1136 the agent reports the /etc/os-release ID (e.g. "wendyos") in the
// Os field rather than "linux", so the identity check must not gate on
// Os == "linux"; the WendyOS-specific signals (version prefix / device type)
// are authoritative on their own.
func TestValidateOSUpdateIdentityAcceptsWendyOSReportedAsDistroID(t *testing.T) {
	strp := func(s string) *string { return &s }
	cases := []*agentpb.GetAgentVersionResponse{
		{Os: "wendyos", OsVersion: strp("WendyOS-0.10.4")},
		{Os: "edgeos", DeviceType: strp("jetson-orin-nano")},
	}
	for _, resp := range cases {
		if err := validateOSUpdateIdentity(resp); err != nil {
			t.Fatalf("validateOSUpdateIdentity(%+v) error = %v, want nil", resp, err)
		}
	}
}

func TestValidateOSUpdateTarget(t *testing.T) {
	strp := func(s string) *string { return &s }

	tests := []struct {
		name string
		resp *agentpb.GetAgentVersionResponse
		want string
	}{
		{
			name: "generic setup is not compatible",
			resp: &agentpb.GetAgentVersionResponse{Os: "darwin"},
			want: osUpdateUnsupportedMessage,
		},
		{
			name: "macOS version does not imply WendyOS",
			resp: &agentpb.GetAgentVersionResponse{Os: "darwin", OsVersion: strp("14.4.1")},
			want: osUpdateUnsupportedMessage,
		},
		{
			name: "linux host with agent is not WendyOS",
			resp: &agentpb.GetAgentVersionResponse{Os: "linux"},
			want: linuxOSUpdateUnsupportedMessage,
		},
		{
			name: "linux host with an update backend is still not WendyOS",
			resp: &agentpb.GetAgentVersionResponse{Os: "linux", Featureset: []string{"wendyos-update"}},
			want: linuxOSUpdateUnsupportedMessage,
		},
		{
			name: "linux OS version does not imply WendyOS",
			resp: &agentpb.GetAgentVersionResponse{Os: "linux", OsVersion: strp("22.04")},
			want: linuxOSUpdateUnsupportedMessage,
		},
		{
			name: "WendyOS without an update backend is unsupported",
			resp: &agentpb.GetAgentVersionResponse{Os: "linux", OsVersion: strp("WendyOS-0.10.4")},
			want: wendyOSMissingUpdaterMessage,
		},
		{
			name: "WendyOS version with wendyos-update is supported",
			resp: &agentpb.GetAgentVersionResponse{Os: "linux", OsVersion: strp("WendyOS-0.10.4"), Featureset: []string{"wendyos-update"}},
		},
		{
			name: "WendyOS device type with wendyos-update is supported",
			resp: &agentpb.GetAgentVersionResponse{Os: "linux", DeviceType: strp("raspberry-pi-5"), Featureset: []string{"wendyos-update"}},
		},
		{
			name: "WendyOS reported as a distro id (post-#1136) is supported",
			resp: &agentpb.GetAgentVersionResponse{Os: "wendyos", OsVersion: strp("WendyOS-0.10.4"), Featureset: []string{"wendyos-update"}},
		},
		{
			name: "WendyOS distro id with device type is supported",
			resp: &agentpb.GetAgentVersionResponse{Os: "edgeos", DeviceType: strp("jetson-orin-nano"), Featureset: []string{"wendyos-update"}},
		},
		{
			name: "generic distro id host still gets the Linux guidance",
			resp: &agentpb.GetAgentVersionResponse{Os: "ubuntu"},
			want: linuxOSUpdateUnsupportedMessage,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOSUpdateTarget(tc.resp)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validateOSUpdateTarget() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateOSUpdateTarget() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Fatalf("validateOSUpdateTarget() error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

func TestHasOTABackend(t *testing.T) {
	tests := []struct {
		name string
		resp *agentpb.GetAgentVersionResponse
		want bool
	}{
		{
			name: "wendyos-update only (e.g. Jetson Orin Nano)",
			resp: &agentpb.GetAgentVersionResponse{Featureset: []string{"gpu", "wendyos-update", "os-healthcheck"}},
			want: true,
		},
		{
			// Regression: a legacy mender-only featureset (old CLI talking to a
			// stale agent) must not be treated as an OTA-capable backend.
			name: "mender only",
			resp: &agentpb.GetAgentVersionResponse{Featureset: []string{"mender"}},
			want: false,
		},
		{
			name: "wendyos-update alongside an unrelated legacy entry",
			resp: &agentpb.GetAgentVersionResponse{Featureset: []string{"wendyos-update", "mender"}},
			want: true,
		},
		{
			name: "no update backend",
			resp: &agentpb.GetAgentVersionResponse{Featureset: []string{"gpu", "audio"}},
			want: false,
		},
		{
			name: "empty featureset",
			resp: &agentpb.GetAgentVersionResponse{},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasOTABackend(tc.resp); got != tc.want {
				t.Fatalf("hasOTABackend() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOSUpdateStackMismatch(t *testing.T) {
	tests := []struct {
		name        string
		features    []string
		artifactURL string
		wantErr     bool
		wantSubstr  string
	}{
		{
			name:        "wendy artifact on a device that predates the wendyos-update stack requires a reflash",
			features:    []string{"os-healthcheck"},
			artifactURL: "https://storage.googleapis.com/img/wendyos-image.rootfs.wendy",
			wantErr:     true,
			wantSubstr:  "reflash",
		},
		{
			name:        "wendy artifact on a wendyos-update device is fine",
			features:    []string{"wendyos-update"},
			artifactURL: "https://storage.googleapis.com/img/wendyos-image.rootfs.wendy",
		},
		{
			name:        "unknown artifact extension is not constrained",
			features:    []string{"os-healthcheck"},
			artifactURL: "https://example.com/custom-artifact",
		},
		{
			name:        "device without advertised backends is left to the agent",
			features:    nil,
			artifactURL: "https://storage.googleapis.com/img/wendyos-image.rootfs.wendy",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := &agentpb.GetAgentVersionResponse{Featureset: tc.features}
			err := osUpdateStackMismatch(resp, tc.artifactURL)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("osUpdateStackMismatch() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("osUpdateStackMismatch() = nil, want error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error %q should contain %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestProgressLabel(t *testing.T) {
	tests := []struct {
		phase   string
		percent int32
		want    string
	}{
		{"installing", 42, "Installing update (42%)"},
		{"installing", 0, "Installing update..."},
		{"downloading", 0, "Downloading update..."},
		{"finalizing", 100, "Finalizing (100%)"},
		{"", 0, "Updating WendyOS..."},
	}
	for _, tc := range tests {
		if got := progressLabel(tc.phase, tc.percent); got != tc.want {
			t.Errorf("progressLabel(%q,%d) = %q, want %q", tc.phase, tc.percent, got, tc.want)
		}
	}
}

// TestClassifyOSRollback pins the boundary between a genuine healthcheck
// failure and an update that never booted (WDY-2200). Every marker corresponds
// to a real wendyos-update rejection message; an unrecognised note must stay
// unknown so the CLI reports no cause rather than the wrong one.
func TestClassifyOSRollback(t *testing.T) {
	failed := []*agentpb.GetOSUpdateStatusResponse_ServiceResult{
		{Unit: "avahi-daemon.service", Status: agentpb.GetOSUpdateStatusResponse_ServiceResult_STATUS_FAILED},
	}
	skipped := []*agentpb.GetOSUpdateStatusResponse_ServiceResult{
		{Unit: "avahi-daemon.service", Status: agentpb.GetOSUpdateStatusResponse_ServiceResult_STATUS_SKIPPED},
	}

	tests := []struct {
		name     string
		services []*agentpb.GetOSUpdateStatusResponse_ServiceResult
		note     string
		want     osRollbackReason
	}{
		{"a failed service is a healthcheck failure", failed, "", rollbackReasonHealthchecks},
		{"a failed service wins over a never-booted note", failed,
			"pending update x is marked failed; run rollback", rollbackReasonHealthchecks},
		{"skipped services are not failures", skipped, "", rollbackReasonUnknown},
		{"marked-failed deployment never booted", nil,
			"wendyos-update commit failed: exit status 1 (pending update x is marked failed; run rollback)", rollbackReasonNotBooted},
		{"firmware fallback never booted", nil,
			"running slot A but the update targeted slot 1 (firmware fallback)", rollbackReasonNotBooted},
		{"written but never swapped never booted", nil,
			"pending update x was written but never swapped; run rollback or mark-good", rollbackReasonNotBooted},
		{"marker matching ignores case", nil, "PENDING UPDATE X IS MARKED FAILED", rollbackReasonNotBooted},
		{"empty record is unknown", nil, "", rollbackReasonUnknown},
		{"unrecognised rejection is unknown", nil,
			"wendyos-update commit failed: exit status 3 (something we have never seen)", rollbackReasonUnknown},
		// engine.HookError formats a gating hook failure as
		// `health hook "<name>" failed: <err>`. That IS a real healthcheck
		// rejection even though no per-service results accompany it, so the
		// delegated path must keep the healthcheck wording rather than degrade
		// to the neutral one.
		{"a health.d hook rejection is a healthcheck failure", nil,
			`wendyos-update commit failed: exit status 4 (health hook "50-containerd.sh" failed: exit status 1)`,
			rollbackReasonHealthchecks},
		{"a platform-verify rejection is neither", nil,
			"wendyos-update commit failed: exit status 4 (platform verify: ESRT status 6163)", rollbackReasonUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOSRollback(tc.services, tc.note); got != tc.want {
				t.Errorf("classifyOSRollback(%v, %q) = %v, want %v", tc.services, tc.note, got, tc.want)
			}
		})
	}
}

func TestFormatOSUpdateStatus(t *testing.T) {
	tests := []struct {
		name            string
		resp            *agentpb.GetOSUpdateStatusResponse
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:         "no record",
			resp:         &agentpb.GetOSUpdateStatusResponse{HasResult: false},
			wantContains: []string{"No OS update"},
		},
		{
			// WDY-2200: this record is what `wendy os update-status` showed for
			// days on a stranded Thor, claiming healthchecks failed when
			// health.d had never run.
			name: "rolled back because the OS never booted does not blame healthchecks",
			resp: &agentpb.GetOSUpdateStatusResponse{
				HasResult:    true,
				Outcome:      agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLED_BACK,
				OldOsVersion: "WendyOS-0.17.0",
				NewOsVersion: "WendyOS-0.17.0",
				Note:         "wendyos-update commit failed: exit status 1 (pending update wendyos-image-jetson-agx-thor-devkit-nvme-wendyos-0.18.2 is marked failed; run rollback)",
			},
			wantContains:    []string{"did not boot", "is marked failed"},
			wantNotContains: []string{"healthcheck"},
		},
		{
			name: "rolled back with an unrecognised reason does not blame healthchecks",
			resp: &agentpb.GetOSUpdateStatusResponse{
				HasResult: true,
				Outcome:   agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLED_BACK,
				Note:      "wendyos-update commit failed: exit status 3 (something we have never seen)",
			},
			wantContains:    []string{"rolled back"},
			wantNotContains: []string{"healthcheck"},
		},
		{
			name: "commit failed shows the captured reason",
			resp: &agentpb.GetOSUpdateStatusResponse{
				HasResult:    true,
				Outcome:      agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMIT_FAILED,
				OldOsVersion: "WendyOS-0.10.4",
				NewOsVersion: "WendyOS-0.11.0",
				Note:         "wendyos-update commit failed: exit status 1 (tegra: ESRT capsule not staged)",
			},
			wantContains: []string{"commit", "ESRT capsule not staged", "WendyOS-0.11.0"},
		},
		{
			name: "rolled back lists failed services",
			resp: &agentpb.GetOSUpdateStatusResponse{
				HasResult: true,
				Outcome:   agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLED_BACK,
				Services: []*agentpb.GetOSUpdateStatusResponse_ServiceResult{
					{Unit: "avahi-daemon.service", Status: agentpb.GetOSUpdateStatusResponse_ServiceResult_STATUS_FAILED, Reason: "timed out"},
				},
			},
			wantContains: []string{"rolled back", "avahi-daemon.service", "timed out"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := formatOSUpdateStatus(tc.resp)
			for _, want := range tc.wantContains {
				if !strings.Contains(msg, want) {
					t.Errorf("formatOSUpdateStatus() = %q, missing %q", msg, want)
				}
			}
			for _, unwanted := range tc.wantNotContains {
				if strings.Contains(strings.ToLower(msg), strings.ToLower(unwanted)) {
					t.Errorf("formatOSUpdateStatus() = %q, must not mention %q", msg, unwanted)
				}
			}
		})
	}
}

// TestOSUpdateStatusError pins the `os update-status` friendly-error fix: a
// Mac agent's Unimplemented response must not tell the user to "update the
// agent first" — no agent update makes OS update status available on macOS,
// since there is no WendyOS OTA there at all. It reuses the fake
// WendyAgentServiceServer from macos_unsupported_test.go, whose embedded
// UnimplementedWendyAgentServiceServer returns a real Unimplemented status for
// GetOSUpdateStatus, giving osUpdateStatusError the exact error shape it sees
// in production.
func TestOSUpdateStatusError(t *testing.T) {
	tests := []struct {
		name         string
		agentOS      string
		wantContains string
		wantExcludes string
	}{
		{
			name:         "darwin agent gets the macOS-beta-unsupported message",
			agentOS:      "darwin",
			wantContains: "current Wendy Agent for macOS beta",
			wantExcludes: "update the agent first",
		},
		{
			name:         "linux agent keeps the update-the-agent message",
			agentOS:      "wendyos",
			wantContains: "update the agent first",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			client := startUnsupportedAgentClient(t, tc.agentOS)

			_, rpcErr := client.GetOSUpdateStatus(ctx, &agentpb.GetOSUpdateStatusRequest{IncludeEngineStatus: true})
			if status.Code(rpcErr) != codes.Unimplemented {
				t.Fatalf("GetOSUpdateStatus code = %s, want Unimplemented", status.Code(rpcErr))
			}

			err := osUpdateStatusError(ctx, client, rpcErr)
			if err == nil {
				t.Fatal("osUpdateStatusError returned nil, want error")
			}
			if !strings.Contains(err.Error(), tc.wantContains) {
				t.Errorf("osUpdateStatusError() = %q, want substring %q", err, tc.wantContains)
			}
			if tc.wantExcludes != "" && strings.Contains(err.Error(), tc.wantExcludes) {
				t.Errorf("osUpdateStatusError() = %q, should not contain %q", err, tc.wantExcludes)
			}
		})
	}
}

// TestOSUpdateStatusErrorNonUnimplementedPassesThrough locks in that a
// non-Unimplemented failure (e.g. a real network error) keeps its own wrapped
// message instead of being reinterpreted as an unsupported-agent case.
func TestOSUpdateStatusErrorNonUnimplementedPassesThrough(t *testing.T) {
	client := startUnsupportedAgentClient(t, "darwin")
	wantErr := status.Error(codes.Unavailable, "connection reset")

	err := osUpdateStatusError(context.Background(), client, wantErr)
	if err == nil {
		t.Fatal("osUpdateStatusError returned nil, want error")
	}
	if !strings.Contains(err.Error(), "querying OS update status") || !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("osUpdateStatusError() = %q, want wrapped query error", err)
	}
}

func TestResolveArtifactPath(t *testing.T) {
	t.Run("direct file is returned regardless of extension", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "update.wendy")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := resolveArtifactPath(f)
		if err != nil {
			t.Fatalf("resolveArtifactPath(%q) error = %v", f, err)
		}
		if got != f {
			t.Fatalf("resolveArtifactPath(%q) = %q, want %q", f, got, f)
		}
	})

	t.Run("directory search finds a .wendy artifact", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "image.wendy")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := resolveArtifactPath(dir)
		if err != nil {
			t.Fatalf("resolveArtifactPath(%q) error = %v", dir, err)
		}
		if got != f {
			t.Fatalf("resolveArtifactPath(%q) = %q, want %q", dir, got, f)
		}
	})

	t.Run("directory search does not find a .mender artifact", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "image.mender")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveArtifactPath(dir); err == nil {
			t.Fatalf("resolveArtifactPath(%q) error = nil, want error", dir)
		}
	})
}

func TestArtifactSuffix(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"wendy artifact", "https://storage.example.com/images/raspberry-pi-5/1.0/wendyos-image-x.rootfs.wendy", ".wendy"},
		{"wendy with query string", "https://storage.example.com/x.wendy?token=abc&exp=123", ".wendy"},
		{"unknown extension falls back to wendy", "https://storage.example.com/images/x.bin", ".wendy"},
		{"bare local path", "/tmp/update.wendy", ".wendy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := artifactSuffix(tc.url); got != tc.want {
				t.Fatalf("artifactSuffix(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

func TestEvaluateOSUpdateOutcome(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-2 * time.Minute).Unix()
	stale := now.Add(-2 * time.Hour).Unix()

	committed := &agentpb.GetOSUpdateStatusResponse{
		HasResult:     true,
		Outcome:       agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMITTED,
		NewOsVersion:  "WendyOS-0.11.0",
		CreatedAtUnix: fresh,
		Services: []*agentpb.GetOSUpdateStatusResponse_ServiceResult{
			{Unit: "avahi-daemon.service", Status: agentpb.GetOSUpdateStatusResponse_ServiceResult_STATUS_HEALTHY},
		},
	}
	rolledBack := &agentpb.GetOSUpdateStatusResponse{
		HasResult:     true,
		Outcome:       agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLED_BACK,
		OldOsVersion:  "WendyOS-0.10.4",
		NewOsVersion:  "WendyOS-0.11.0",
		CreatedAtUnix: fresh,
		Services: []*agentpb.GetOSUpdateStatusResponse_ServiceResult{
			{Unit: "avahi-daemon.service", Status: agentpb.GetOSUpdateStatusResponse_ServiceResult_STATUS_FAILED, Reason: "timed out after 30s waiting for active"},
			{Unit: "containerd.service", Status: agentpb.GetOSUpdateStatusResponse_ServiceResult_STATUS_HEALTHY},
		},
	}
	rollbackFailed := &agentpb.GetOSUpdateStatusResponse{
		HasResult:     true,
		Outcome:       agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLBACK_FAILED,
		CreatedAtUnix: fresh,
		RollbackError: "wendyos-update reported nothing to roll back",
		Services: []*agentpb.GetOSUpdateStatusResponse_ServiceResult{
			{Unit: "avahi-daemon.service", Status: agentpb.GetOSUpdateStatusResponse_ServiceResult_STATUS_FAILED, Reason: "timed out"},
		},
	}
	commitFailed := &agentpb.GetOSUpdateStatusResponse{
		HasResult:     true,
		Outcome:       agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMIT_FAILED,
		CreatedAtUnix: fresh,
		Note:          "wendyos-update commit failed: exit status 1 (tegra: ESRT capsule not staged)",
	}
	// A delegated (wendyos-update health.d) rollback has no per-service results;
	// the reason is carried in Note and must still reach the user.
	delegatedRolledBack := &agentpb.GetOSUpdateStatusResponse{
		HasResult:     true,
		Outcome:       agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLED_BACK,
		OldOsVersion:  "WendyOS-0.10.4",
		CreatedAtUnix: fresh,
		Note:          "wendyos-update commit failed: exit status 1 (pending update is marked failed; run rollback)",
	}
	// A rollback-failed record can also carry the commit-rejection reason in
	// Note; it must not be dropped alongside RollbackError.
	rollbackFailedWithNote := &agentpb.GetOSUpdateStatusResponse{
		HasResult:     true,
		Outcome:       agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLBACK_FAILED,
		CreatedAtUnix: fresh,
		Note:          "wendyos-update commit failed: exit status 1 (pending update is marked failed; run rollback)",
		RollbackError: "wendyos-update reported nothing to roll back",
	}

	// A rollback whose commit rejection matches none of the recognised
	// "never booted" signatures: the CLI cannot tell why, so it must not
	// invent a cause.
	unclassifiedRolledBack := &agentpb.GetOSUpdateStatusResponse{
		HasResult:     true,
		Outcome:       agentpb.GetOSUpdateStatusResponse_OUTCOME_ROLLED_BACK,
		OldOsVersion:  "WendyOS-0.10.4",
		CreatedAtUnix: fresh,
		Note:          "wendyos-update commit failed: exit status 3 (something we have never seen)",
	}

	tests := []struct {
		name            string
		resp            *agentpb.GetOSUpdateStatusResponse
		rpcErr          error
		preVer          string
		postVer         string
		wantErr         bool
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:         "committed is verified success",
			resp:         committed,
			preVer:       "WendyOS-0.10.4",
			postVer:      "WendyOS-0.11.0",
			wantErr:      false,
			wantContains: []string{"verified"},
		},
		{
			name:    "committed for a version the device is not running is rejected",
			resp:    committed,
			preVer:  "WendyOS-0.10.4",
			postVer: "WendyOS-0.10.4",
			wantErr: true,
			wantContains: []string{
				"WendyOS-0.11.0",
				"WendyOS-0.10.4",
			},
		},
		{
			name:         "committed with unknown running version is trusted",
			resp:         committed,
			preVer:       "WendyOS-0.10.4",
			postVer:      "",
			wantErr:      false,
			wantContains: []string{"verified", "WendyOS-0.11.0"},
		},
		{
			name:    "rolled back reports failed services",
			resp:    rolledBack,
			preVer:  "WendyOS-0.10.4",
			postVer: "WendyOS-0.10.4",
			wantErr: true,
			wantContains: []string{
				"rolled back",
				"avahi-daemon.service",
				"timed out after 30s",
				"WendyOS-0.10.4",
			},
		},
		{
			name:    "delegated rollback surfaces the note when there are no service results",
			resp:    delegatedRolledBack,
			preVer:  "WendyOS-0.10.4",
			postVer: "WendyOS-0.10.4",
			wantErr: true,
			wantContains: []string{
				"rolled back",
				"WendyOS-0.10.4",
				"is marked failed",
			},
		},
		{
			// WDY-2200: the boot verifier marks the deployment failed before
			// commit, so health.d never runs. Blaming healthchecks sent a real
			// investigation to the wrong layer entirely.
			name:    "rollback for an OS that never booted does not blame healthchecks",
			resp:    delegatedRolledBack,
			preVer:  "WendyOS-0.10.4",
			postVer: "WendyOS-0.10.4",
			wantErr: true,
			wantContains: []string{
				"did not boot",
				"WendyOS-0.10.4",
			},
			wantNotContains: []string{"healthcheck"},
		},
		{
			name:            "rollback with an unrecognised reason does not blame healthchecks",
			resp:            unclassifiedRolledBack,
			preVer:          "WendyOS-0.10.4",
			postVer:         "WendyOS-0.10.4",
			wantErr:         true,
			wantContains:    []string{"rolled back", "something we have never seen"},
			wantNotContains: []string{"healthcheck"},
		},
		{
			// The agent-run CheckAll path genuinely is a healthcheck failure,
			// so that wording must survive.
			name:         "rollback with failed services still reports healthchecks",
			resp:         rolledBack,
			preVer:       "WendyOS-0.10.4",
			postVer:      "WendyOS-0.10.4",
			wantErr:      true,
			wantContains: []string{"healthcheck", "avahi-daemon.service"},
		},
		{
			name:            "rollback-failed for an OS that never booted does not blame healthchecks",
			resp:            rollbackFailedWithNote,
			preVer:          "WendyOS-0.10.4",
			postVer:         "WendyOS-0.11.0",
			wantErr:         true,
			wantContains:    []string{"did not boot", "nothing to roll back"},
			wantNotContains: []string{"healthcheck"},
		},
		{
			name:         "rollback failed reports degraded state",
			resp:         rollbackFailed,
			preVer:       "WendyOS-0.10.4",
			postVer:      "WendyOS-0.11.0",
			wantErr:      true,
			wantContains: []string{"avahi-daemon.service", "nothing to roll back"},
		},
		{
			name:    "rollback failed surfaces the note alongside the rollback error",
			resp:    rollbackFailedWithNote,
			preVer:  "WendyOS-0.10.4",
			postVer: "WendyOS-0.11.0",
			wantErr: true,
			wantContains: []string{
				"is marked failed",
				"nothing to roll back",
			},
		},
		{
			name:         "commit failed surfaces the captured reason",
			resp:         commitFailed,
			preVer:       "WendyOS-0.10.4",
			postVer:      "WendyOS-0.11.0",
			wantErr:      true,
			wantContains: []string{"commit", "ESRT capsule not staged"},
		},
		{
			name:         "unimplemented with unchanged version warns of rollback",
			rpcErr:       status.Error(codes.Unimplemented, "unknown method"),
			preVer:       "WendyOS-0.10.4",
			postVer:      "WendyOS-0.10.4",
			wantErr:      true,
			wantContains: []string{"WendyOS-0.10.4"},
		},
		{
			name:         "unimplemented with changed version succeeds without verification",
			rpcErr:       status.Error(codes.Unimplemented, "unknown method"),
			preVer:       "WendyOS-0.10.4",
			postVer:      "WendyOS-0.11.0",
			wantErr:      false,
			wantContains: []string{"WendyOS-0.11.0"},
		},
		{
			name:    "no record with changed version succeeds without verification",
			resp:    &agentpb.GetOSUpdateStatusResponse{HasResult: false},
			preVer:  "WendyOS-0.10.4",
			postVer: "WendyOS-0.11.0",
			wantErr: false,
		},
		{
			name: "stale record falls back to version comparison",
			resp: &agentpb.GetOSUpdateStatusResponse{
				HasResult:     true,
				Outcome:       agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMITTED,
				CreatedAtUnix: stale,
			},
			preVer:  "WendyOS-0.10.4",
			postVer: "WendyOS-0.10.4",
			wantErr: true,
		},
		{
			name:         "unknown post version cannot verify but does not fail",
			resp:         &agentpb.GetOSUpdateStatusResponse{HasResult: false},
			preVer:       "WendyOS-0.10.4",
			postVer:      "",
			wantErr:      false,
			wantContains: []string{"could not be verified"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := evaluateOSUpdateOutcome(tc.resp, tc.rpcErr, tc.preVer, tc.postVer, now)
			if tc.wantErr && err == nil {
				t.Fatalf("error = nil, want non-nil; msg = %q", msg)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("error = %v, want nil; msg = %q", err, msg)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q missing %q", msg, want)
				}
			}
			for _, unwanted := range tc.wantNotContains {
				if strings.Contains(strings.ToLower(msg), strings.ToLower(unwanted)) {
					t.Errorf("message %q must not mention %q", msg, unwanted)
				}
			}
			if err != nil {
				for _, unwanted := range tc.wantNotContains {
					if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(unwanted)) {
						t.Errorf("error %q must not mention %q", err, unwanted)
					}
				}
			}
		})
	}
}

func TestIsLoopbackHostIdentifiesAPortForwardedDevice(t *testing.T) {
	// A VM answers on the host's loopback through a port forward. It reports no
	// WiFi but is not offline, and the local artifact server could only ever
	// advertise a loopback address, which inside the guest is the guest.
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1:50051", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"[::1]:50051", true},
		{"localhost:50051", true},
		{"192.168.2.253:50051", false},
		{"169.254.198.132", false},
		{"rpi5.local:50051", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isLoopbackHost(tc.host); got != tc.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
