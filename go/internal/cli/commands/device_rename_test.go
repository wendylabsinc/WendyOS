package commands

import (
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestValidateHostnameArg(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"simple", "wendy", false},
		{"with prefix", "wendyos-living-room", false},
		{"single letter", "a", false},
		{"digits and hyphens", "node-1-of-3", false},
		{"max length 63", strings.Repeat("a", 63), false},

		{"empty", "", true},
		{"too long 64", strings.Repeat("a", 64), true},
		{"starts with digit", "1wendy", true},
		{"starts with hyphen", "-wendy", true},
		{"ends with hyphen", "wendy-", true},
		{"uppercase", "Wendy", true},
		{"underscore", "wendy_os", true},
		{"dot", "wendy.os", true},
		{"space", "wendy os", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHostnameArg(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateHostnameArg(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestResolveRenameNameFromArg(t *testing.T) {
	got, err := resolveRenameName([]string{"  living-room  "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "living-room" {
		t.Errorf("resolveRenameName trimmed = %q, want %q", got, "living-room")
	}

	if _, err := resolveRenameName([]string{"Bad_Name"}); err == nil {
		t.Errorf("resolveRenameName with invalid arg: expected error, got nil")
	}
}

func TestRepointDefaultDeviceFollowsARenamedMDNSDefault(t *testing.T) {
	origFlag := deviceFlag
	t.Cleanup(func() { deviceFlag = origFlag })
	deviceFlag = ""

	setTempConfig(t, &config.Config{
		DefaultDevice: "old-name.local",
		DevicePins:    map[string]config.DevicePin{"old-name": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"}},
	})
	repointDefaultDevice("new-name")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultDevice != "new-name.local" {
		t.Fatalf("DefaultDevice = %q, want new-name.local", cfg.DefaultDevice)
	}
	if _, ok := cfg.DevicePinFor("old-name"); !ok {
		t.Fatal("repointing the default dropped an unrelated field")
	}

	setTempConfig(t, &config.Config{DefaultDevice: "192.168.1.5"})
	repointDefaultDevice("new-name")
	if cfg, _ := config.Load(); cfg.DefaultDevice != "192.168.1.5" {
		t.Fatalf("an IP default was repointed to %q", cfg.DefaultDevice)
	}
}
