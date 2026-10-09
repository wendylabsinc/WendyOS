package vm

import (
	"slices"
	"strings"
	"testing"
)

func TestLiteSpecUsesFirmwareAndPrivateNetwork(t *testing.T) {
	t.Setenv("WENDY_LITE_DNS", "")
	s := Spec{Name: "lite", Profile: ProfileWendyLite, DiskPath: "/tmp/private flash.bin", Net: NetConfig{Mode: NetUser, AgentPort: 55540}}
	args, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	if s.Binary() != "esp-emu" || !slices.Contains(args, s.DiskPath) || !slices.Contains(args, "--save-state") || !slices.Contains(args, "user,hostfwd=tcp:127.0.0.1:55540-:5054") {
		t.Fatalf("args: %v", args)
	}
	if strings.Contains(strings.Join(args, " "), "pflash") {
		t.Fatal("Lite received ARM firmware")
	}
	s.Net.Mode = NetShared
	if _, err := s.Args(); err == nil {
		t.Fatal("shared networking accepted")
	}
	s.Net.Mode = NetUser
	s.Net.AgentPort = 0
	if _, err := s.Args(); err == nil {
		t.Fatal("missing port accepted")
	}
}

func TestNativeLiteUsesESPEmulator(t *testing.T) {
	s := Spec{Name: "native", Profile: ProfileWendyLiteNative, ESPEmulatorPath: "/tmp/esp-emu", DiskPath: "/tmp/native.bin", Net: NetConfig{Mode: NetUser, AgentPort: 55540}}
	if s.Binary() != "/tmp/esp-emu" {
		t.Fatal("native simulator did not select its emulator")
	}
	args, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "esp32c6") || !slices.Contains(args, "--save-state") {
		t.Fatalf("args: %v", args)
	}
}

func TestLiteDNSOverride(t *testing.T) {
	s := Spec{Name: "lite", Profile: ProfileWendyLite, DiskPath: "/tmp/flash.bin", Net: NetConfig{Mode: NetUser, AgentPort: 55540}}
	for _, dns := range []string{"1.1.1.1", "2001:4860:4860::8888"} {
		t.Setenv("WENDY_LITE_DNS", dns)
		args, err := s.Args()
		if err != nil || !slices.Contains(args, "user,hostfwd=tcp:127.0.0.1:55540-:5054,dns="+dns) {
			t.Fatalf("DNS %q: args=%v err=%v", dns, args, err)
		}
	}
	for _, dns := range []string{"resolver.example", "1.1.1.1,restrict=no", "0.0.0.0", "224.0.0.1", "fe80::1%en0"} {
		t.Setenv("WENDY_LITE_DNS", dns)
		if _, err := s.Args(); err == nil {
			t.Fatalf("accepted invalid resolver %q", dns)
		}
	}
}
