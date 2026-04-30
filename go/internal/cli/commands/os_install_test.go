//go:build darwin || linux || windows

package commands

import (
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/internal/shared/version"
)

func TestNewOSInstallCmd_Flags(t *testing.T) {
	cmd := newOSInstallCmd()
	if cmd.Use != "install [image] [drive]" {
		t.Errorf("Use = %q; want %q", cmd.Use, "install [image] [drive]")
	}

	expectedFlags := []string{"nightly", "force", "device-type", "version", "drive", "wifi-ssid", "wifi-password", "wifi", "no-wifi", "device-name"}
	for _, name := range expectedFlags {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing flag %q", name)
		}
	}
}

func TestNewOSInstallCmd_NightlyVersionMutualExclusion(t *testing.T) {
	cmd := newOSInstallCmd()
	cmd.SetArgs([]string{"--nightly", "--version", "0.10.0"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --nightly and --version are both set")
	}
	if got := err.Error(); got != "--nightly and --version are mutually exclusive" {
		t.Errorf("unexpected error: %q", got)
	}
}

func TestNewOSInstallCmd_PositionalArgsIncompatibleWithFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"positional with --device-type", []string{"image.img", "/dev/disk4", "--device-type", "raspberry-pi-5", "--force"}},
		{"positional with --version", []string{"image.img", "/dev/disk4", "--version", "0.10.0", "--force"}},
		{"positional with --drive", []string{"image.img", "/dev/disk4", "--drive", "/dev/disk5", "--force"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newOSInstallCmd()
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error when positional args are combined with manifest flags")
			}
			expected := "positional [image] [drive] arguments cannot be combined with --device-type, --version, --drive, --wifi-ssid, --wifi-password, --wifi, --no-wifi, or --device-name"
			if got := err.Error(); got != expected {
				t.Errorf("unexpected error: %q; want %q", got, expected)
			}
		})
	}
}

func TestNewOSInstallCmd_SinglePositionalArgRejected(t *testing.T) {
	cmd := newOSInstallCmd()
	cmd.SetArgs([]string{"image.img"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when exactly 1 positional arg is provided")
	}
	expected := "positional arguments must be provided as [image] [drive]; got 1 argument"
	if got := err.Error(); got != expected {
		t.Errorf("unexpected error: %q; want %q", got, expected)
	}
}

func TestNewOSInstallCmd_ESP32DeviceTypeRejected(t *testing.T) {
	for _, dt := range []string{"esp32-c6", "esp32-c5"} {
		t.Run(dt, func(t *testing.T) {
			cmd := newOSInstallCmd()
			cmd.SetArgs([]string{"--device-type", dt})
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error for ESP32 --device-type")
			}
			if !strings.Contains(err.Error(), "does not support ESP32") {
				t.Errorf("unexpected error: %q", err.Error())
			}
		})
	}
}

func TestPickManifestVersion_SemverOrdering(t *testing.T) {
	// Verify that version keys are sorted semantically, not lexicographically.
	// "0.10.0" should come after "0.9.0" semantically but before it lexicographically.
	versions := []string{"0.2.0", "0.10.0", "0.9.0", "0.1.0", "0.10.1"}

	// Use the same sorting logic as pickManifestVersion.
	sorted := make([]string, len(versions))
	copy(sorted, versions)
	sortFunc := func(i, j int) bool {
		return version.CompareVersions(sorted[i], sorted[j]) > 0
	}

	// Simple bubble sort for testing.
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if !sortFunc(i, j) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	expected := []string{"0.10.1", "0.10.0", "0.9.0", "0.2.0", "0.1.0"}
	for i, v := range sorted {
		if v != expected[i] {
			t.Errorf("sorted[%d] = %q; want %q (full: %v)", i, v, expected[i], sorted)
			break
		}
	}
}

func TestOsCachedImagePath_Sanitization(t *testing.T) {
	// Valid inputs should produce a valid path.
	path, err := osCachedImagePath("raspberry-pi-5", "0.10.4", StorageUnknown)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty path")
	}

	// Storage suffix is appended for multi-storage devices.
	nvmePath, err := osCachedImagePath("jetson-orin-nano", "0.10.4", StorageNVMe)
	if err != nil {
		t.Fatalf("unexpected error for nvme path: %v", err)
	}
	sdPath, err := osCachedImagePath("jetson-orin-nano", "0.10.4", StorageSD)
	if err != nil {
		t.Fatalf("unexpected error for sd path: %v", err)
	}
	if nvmePath == sdPath {
		t.Fatal("expected distinct cache paths for nvme and sd")
	}

	// Path traversal in version should be rejected.
	_, err = osCachedImagePath("raspberry-pi-5", "../../../etc/passwd", StorageUnknown)
	if err == nil {
		t.Fatal("expected error for path traversal in version")
	}

	// Path traversal in device key should be rejected.
	_, err = osCachedImagePath("../evil", "0.10.4", StorageUnknown)
	if err == nil {
		t.Fatal("expected error for path traversal in device key")
	}
}

func TestParseWiFiEntry(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantSSID string
		wantPW   string
		wantPri  int32
		wantHid  bool
		wantSec  string
		wantErr  bool
	}{
		{"ssid only", "ssid=Home", "Home", "", 0, false, "", false},
		{"all fields", "ssid=Home,password=p,priority=10,hidden=true,security=wpa3", "Home", "p", 10, true, "wpa3", false},
		{"escaped comma", `ssid=My\,Net,password=x`, "My,Net", "x", 0, false, "", false},
		{"missing ssid", "password=p", "", "", 0, false, "", true},
		{"bad priority", "ssid=A,priority=nope", "", "", 0, false, "", true},
		{"unknown key", "ssid=A,foo=bar", "", "", 0, false, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseWiFiEntry(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", c)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.SSID != tc.wantSSID || c.Password != tc.wantPW || c.Priority != tc.wantPri || c.Hidden != tc.wantHid || c.Security != tc.wantSec {
				t.Errorf("got %+v; want ssid=%q pw=%q pri=%d hidden=%v sec=%q",
					c, tc.wantSSID, tc.wantPW, tc.wantPri, tc.wantHid, tc.wantSec)
			}
		})
	}
}

func TestResolveWiFiCredentialsListFlags(t *testing.T) {
	// --wifi-ssid + --wifi-password shortcut (non-TTY path: isInteractiveTerminal returns false in tests).
	creds, err := resolveWiFiCredentialsList(wifiCLIOptions{SSID: "Home", Password: "pw"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(creds) != 1 || creds[0].SSID != "Home" || creds[0].Password != "pw" {
		t.Errorf("shortcut produced %+v", creds)
	}

	// Repeatable --wifi: order preserved, priorities honoured.
	creds, err = resolveWiFiCredentialsList(wifiCLIOptions{Entries: []string{
		"ssid=First,password=a,priority=100",
		"ssid=Second,priority=50",
		"ssid=Hidden,hidden=true",
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(creds) != 3 {
		t.Fatalf("got %d creds; want 3", len(creds))
	}
	if creds[0].SSID != "First" || creds[0].Priority != 100 {
		t.Errorf("creds[0] = %+v", creds[0])
	}
	if creds[2].SSID != "Hidden" || !creds[2].Hidden {
		t.Errorf("creds[2] = %+v", creds[2])
	}

	// --no-wifi short-circuits even when other flags are empty.
	creds, err = resolveWiFiCredentialsList(wifiCLIOptions{NoWifi: true})
	if err != nil || creds != nil {
		t.Errorf("no-wifi: got %v, %+v", err, creds)
	}

	// --no-wifi combined with --wifi-ssid should error.
	if _, err := resolveWiFiCredentialsList(wifiCLIOptions{NoWifi: true, SSID: "Home"}); err == nil {
		t.Error("expected error when --no-wifi is combined with --wifi-ssid")
	}

	// --wifi-password without --wifi-ssid should error.
	if _, err := resolveWiFiCredentialsList(wifiCLIOptions{Password: "pw"}); err == nil {
		t.Error("expected error when --wifi-password is passed alone")
	}
}
