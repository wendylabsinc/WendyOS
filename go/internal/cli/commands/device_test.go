package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestFormatKernelLogRecord(t *testing.T) {
	cases := []struct {
		name string
		rec  *agentpb.KernelLogRecord
		want string
	}{
		{
			name: "sub-second timestamp zero-pads microseconds",
			rec:  &agentpb.KernelLogRecord{TimestampUs: 12_345678, Level: 6, Message: "usb 1-1: new device"},
			want: "[   12.345678] usb 1-1: new device",
		},
		{
			name: "boot instant",
			rec:  &agentpb.KernelLogRecord{TimestampUs: 0, Level: 5, Message: "Linux version 6.1"},
			want: "[    0.000000] Linux version 6.1",
		},
		{
			name: "large uptime keeps full seconds width",
			rec:  &agentpb.KernelLogRecord{TimestampUs: 123456_000007, Level: 4, Message: "late message"},
			want: "[123456.000007] late message",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatKernelLogRecord(tc.rec); got != tc.want {
				t.Errorf("formatKernelLogRecord() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDeviceOSLogsFollowFlag(t *testing.T) {
	f := newDeviceOSLogsCmd().Flags().Lookup("follow")
	if f == nil {
		t.Fatal("expected --follow flag on device os-logs command")
	}
	// Follow defaults to true: the kernel dump tails unless explicitly disabled.
	if f.DefValue != "true" {
		t.Errorf("--follow default = %q, want \"true\"", f.DefValue)
	}
	if f.Shorthand != "f" {
		t.Errorf("--follow shorthand = %q, want \"f\"", f.Shorthand)
	}
}

// The kernel ring buffer is now its own `os-logs` command, not a flag on `logs`.
// Guard against the flags drifting back onto `logs`.
func TestDeviceLogsHasNoKernelFlags(t *testing.T) {
	flags := newDeviceLogsCmd().Flags()
	for _, name := range []string{"os", "follow"} {
		if flags.Lookup(name) != nil {
			t.Errorf("device logs should not have --%s flag; use `device os-logs`", name)
		}
	}
}

func TestDeviceLogsNoFollowFlag(t *testing.T) {
	f := newDeviceLogsCmd().Flags().Lookup("no-follow")
	if f == nil {
		t.Fatal("device logs needs --no-follow for a finite log sample")
	}
	if f.DefValue != "false" {
		t.Errorf("--no-follow default = %q, want false", f.DefValue)
	}
}

func TestDeviceCmd_HasPs(t *testing.T) {
	cmd := newDeviceCmd()
	var found bool
	for _, sub := range cmd.Commands() {
		if sub.Use == "ps" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'ps' subcommand on device command")
	}
}

// TestDeviceCmd_HasHiddenListAlias verifies `wendy device list` exists as a
// hidden alias for `wendy discover`: it must be registered, hidden from the
// help listing, and carry the discover command's flags (proving it delegates
// to the same flow rather than being a stub).
func TestDeviceCmd_HasHiddenListAlias(t *testing.T) {
	cmd := newDeviceCmd()
	var list *cobra.Command
	for _, sub := range cmd.Commands() {
		if sub.Name() == "list" {
			list = sub
			break
		}
	}
	if list == nil {
		t.Fatal("expected hidden 'list' subcommand on device command")
	}
	if !list.Hidden {
		t.Error("'device list' should be hidden")
	}
	// The alias reuses the discover command, so its distinctive --type flag
	// must be present.
	if list.Flags().Lookup("type") == nil {
		t.Error("'device list' should expose discover's --type flag")
	}
}

func TestDefaultEnrollmentName(t *testing.T) {
	cases := map[string]string{
		"playful-reed.local": "playful-reed",
		"playful-reed":       "playful-reed",
		"192.168.1.50":       "",
		"":                   "",
	}
	for in, want := range cases {
		if got := defaultEnrollmentName(in); got != want {
			t.Errorf("defaultEnrollmentName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaybeCheckOSUpdateSkips(t *testing.T) {
	strp := func(s string) *string { return &s }

	tests := []struct {
		name    string
		version *agentpb.GetAgentVersionResponse
	}{
		{"nil version", nil},
		{"non-wendyos darwin", &agentpb.GetAgentVersionResponse{Os: "darwin", OsVersion: strp("14.4")}},
		{"wendyos without an OTA backend",
			&agentpb.GetAgentVersionResponse{Os: "linux", OsVersion: strp("WendyOS-0.10.4"), DeviceType: strp("raspberry-pi-5")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// These inputs must return from the cheap pre-reconnect gate, before
			// any reconnect/manifest/network call. (WendyOS devices with an OTA
			// backend do reconnect to re-read the version, so they're not covered
			// here.) A nil connection is safe because the gate returns before it
			// is used.
			outcome, err := maybeCheckOSUpdate(context.Background(), tc.version, nil, false, false, "", 0)
			if err != nil {
				t.Fatalf("maybeCheckOSUpdate() error = %v, want nil", err)
			}
			if outcome.applied || outcome.online {
				t.Fatalf("maybeCheckOSUpdate() outcome = %+v, want zero (skipped)", outcome)
			}
		})
	}
}

func TestMaybeCheckOSUpdateExplicitArtifactFailsClosed(t *testing.T) {
	strp := func(s string) *string { return &s }
	const artifact = "https://example.com/image.wendy"
	tests := []struct {
		name    string
		version *agentpb.GetAgentVersionResponse
		want    string
	}{
		{"missing version", nil, "device version is unavailable"},
		{"non-WendyOS host", &agentpb.GetAgentVersionResponse{Os: "darwin", OsVersion: strp("14.4")}, "not a WendyOS OTA target"},
		{"missing OTA backend", &agentpb.GetAgentVersionResponse{OsVersion: strp("WendyOS-0.19.3")}, "not a WendyOS OTA target with an update backend"},
		{"reconnect unavailable", &agentpb.GetAgentVersionResponse{OsVersion: strp("WendyOS-0.19.3"), Featureset: []string{"wendyos-update"}}, "reconnecting for the requested OS update"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := maybeCheckOSUpdate(context.Background(), tc.version, nil, false, true, artifact, 0)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("explicit artifact error = %v, want %q", err, tc.want)
			}
		})
	}
}
