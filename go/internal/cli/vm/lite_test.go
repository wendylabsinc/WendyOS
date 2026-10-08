package vm

import (
	"slices"
	"strings"
	"testing"
)

func TestLiteSpecUsesFirmwareAndPrivateNetwork(t *testing.T) {
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
