package vm

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func testRobotProfile(t *testing.T) RobotProfile {
	t.Helper()
	p, err := NewGo2RobotProfile("sha256:"+strings.Repeat("a", 64), "unitree-go2-walking-v1")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRobotProfileKindsPreserveIndependentDesiredIdentities(t *testing.T) {
	s := newTestStore(t)
	go2 := testRobotProfile(t)
	g1, err := NewG1RobotProfile("sha256:"+strings.Repeat("b", 64), "g1-test-policy-v1")
	if err != nil {
		t.Fatal(err)
	}
	if g1.Kind != RobotKindG1 || g1.SourceDigest == go2.SourceDigest || g1.PolicyBundle == go2.PolicyBundle {
		t.Fatalf("G1 reused Go2 identity: %+v", g1)
	}
	for _, profile := range []RobotProfile{go2, g1} {
		createTestVM(t, s, profile.Kind, Meta{ImageVersion: "test-image"})
		if err := s.CreateRobotProfile(profile.Kind, profile); err != nil {
			t.Fatal(err)
		}
		if got, exists, err := s.ReadRobotProfile(profile.Kind); err != nil || !exists || got != profile {
			t.Fatalf("%s profile did not round-trip: %+v %t %v", profile.Kind, got, exists, err)
		}
		if profile.Version != 1 || profile.World != "indoor" || profile.Interface != "lo" || profile.DomainID != 0 || profile.ClockMode != "device" || profile.CPUs != 4 || profile.MemoryMiB != 4096 {
			t.Fatalf("%s changed the VM isolation/resource defaults: %+v", profile.Kind, profile)
		}
	}
	if err := s.CreateRobotProfile(RobotKindGo2, g1); !errors.Is(err, ErrRobotProfileExists) {
		t.Fatalf("G1 configuration replaced an existing Go2 profile: %v", err)
	}
	if got, _, err := s.ReadRobotProfile(RobotKindGo2); err != nil || got != go2 {
		t.Fatalf("Go2 profile changed while configuring G1: %+v %v", got, err)
	}
	if _, err := NewRobotProfile("other", go2.SourceDigest, go2.PolicyBundle); err == nil {
		t.Fatal("constructor accepted an unsupported robot kind")
	}
}

func TestRobotProfilePersistsDesiredConfigurationApartFromRunState(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "robot", Meta{ImageVersion: "test-image"})
	if _, exists, err := s.ReadRobotProfile("robot"); exists || err != nil {
		t.Fatalf("ordinary VM profile: exists=%v err=%v", exists, err)
	}
	p := testRobotProfile(t)
	if p.Version != 1 || p.Kind != "go2" || p.Interface != "lo" || p.DomainID != 0 || p.World != "indoor" || p.ClockMode != "device" || p.VisualDetail != "balanced" || p.CPUs != 4 || p.MemoryMiB != 4096 {
		t.Fatalf("unexpected defaults: %+v", p)
	}
	if err := s.CreateRobotProfile("robot", p); err != nil {
		t.Fatal(err)
	}
	if got, exists, err := s.ReadRobotProfile("robot"); err != nil || !exists || got != p {
		t.Fatalf("profile round trip: %+v %v %v", got, exists, err)
	}
	if _, err := os.Stat(s.StatePath("robot")); !os.IsNotExist(err) {
		t.Fatalf("desired robot configuration wrote run state: %v", err)
	}
	info, err := os.Stat(s.RobotProfilePath("robot"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("profile file protection: %v %v", info, err)
	}
	if err := s.CreateRobotProfile("robot", p); !errors.Is(err, ErrRobotProfileExists) {
		t.Fatalf("second creation: %v", err)
	}
	if err := s.UpdateRobotProfile("robot", func(p *RobotProfile) error {
		p.RuntimeDigest = "sha256:" + strings.Repeat("b", 64)
		p.CPUs, p.MemoryMiB, p.Seed = 8, 8192, 42
		p.VisualDetail = "full"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.ReadRobotProfile("robot")
	if err != nil || got.CPUs != 8 || got.SourceDigest != p.SourceDigest || got.RuntimeDigest == "" || got.PolicyBundle != p.PolicyBundle {
		t.Fatalf("update lost desired state: %+v %v", got, err)
	}
	if err := s.Remove("robot"); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := s.ReadRobotProfile("robot"); exists || err != nil {
		t.Fatalf("removed robot still has a profile: %v %v", exists, err)
	}
	if err := s.UpdateRobotProfile("robot", func(*RobotProfile) error { return nil }); !errors.Is(err, ErrRobotProfileMissing) {
		t.Fatalf("update recreated removed profile: %v", err)
	}
}

func TestRobotProfileRejectsUnsupportedOrUnsafeConfiguration(t *testing.T) {
	for name, change := range map[string]func(*RobotProfile){
		"version":        func(p *RobotProfile) { p.Version = 2 },
		"kind":           func(p *RobotProfile) { p.Kind = "physical-go2" },
		"missing-source": func(p *RobotProfile) { p.SourceDigest = "" },
		"source":         func(p *RobotProfile) { p.SourceDigest = "sha256:bad" },
		"runtime":        func(p *RobotProfile) { p.RuntimeDigest = "latest" },
		"world":          func(p *RobotProfile) { p.World = "../world" },
		"bundle":         func(p *RobotProfile) { p.PolicyBundle = "../policy" },
		"interface":      func(p *RobotProfile) { p.Interface = "eth0" },
		"domain":         func(p *RobotProfile) { p.DomainID = 1 },
		"clock":          func(p *RobotProfile) { p.ClockMode = "unknown" },
		"visual":         func(p *RobotProfile) { p.VisualDetail = "unknown" },
		"cpus":           func(p *RobotProfile) { p.CPUs = 0 },
		"memory":         func(p *RobotProfile) { p.MemoryMiB = -1 },
		"port":           func(p *RobotProfile) { p.SandboxHostPort = 65536 },
	} {
		t.Run(name, func(t *testing.T) {
			p := testRobotProfile(t)
			change(&p)
			if err := p.Validate(); err == nil {
				t.Fatalf("accepted unsupported profile: %+v", p)
			}
		})
	}
}

func TestRobotProfileNeverOverwritesUnreadableOrUnknownRecord(t *testing.T) {
	for name, raw := range map[string][]byte{
		"corrupt":          []byte("{"),
		"unknown-version":  []byte(`{"version":2,"kind":"go2"}`),
		"unknown-field":    []byte(`{"version":1,"kind":"go2","future":true}`),
		"multiple-objects": []byte(`{} {}`),
		"oversize":         bytes.Repeat([]byte(" "), maxRobotProfileBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			createTestVM(t, s, "robot", Meta{})
			if err := os.WriteFile(s.RobotProfilePath("robot"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, exists, err := s.ReadRobotProfile("robot"); !exists || err == nil {
				t.Fatalf("unreadable record mistaken for ordinary VM: %v %v", exists, err)
			}
			if err := s.CreateRobotProfile("robot", testRobotProfile(t)); err == nil {
				t.Fatal("creation replaced unsupported record")
			}
			called := false
			if err := s.UpdateRobotProfile("robot", func(*RobotProfile) error { called = true; return nil }); err == nil || called {
				t.Fatalf("update used unsupported record: callback=%v err=%v", called, err)
			}
			got, err := os.ReadFile(s.RobotProfilePath("robot"))
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("unsupported record changed: %v", err)
			}
		})
	}
}

func TestRobotProfileUpdateSerializesAndRollsBackValidationFailures(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "robot", Meta{})
	p := testRobotProfile(t)
	if err := s.CreateRobotProfile("robot", p); err != nil {
		t.Fatal(err)
	}
	other := &Store{Root: s.Root}
	err := s.UpdateRobotProfile("robot", func(current *RobotProfile) error {
		if err := other.UpdateRobotProfile("robot", func(*RobotProfile) error { return nil }); !errors.Is(err, ErrLifecycleBusy) {
			t.Errorf("competing profile update = %v", err)
		}
		if err := other.Remove("robot"); !errors.Is(err, ErrLifecycleBusy) {
			t.Errorf("removal during profile update = %v", err)
		}
		current.DomainID = 42
		return nil
	})
	if err == nil {
		t.Fatal("invalid update succeeded")
	}
	if got, _, err := s.ReadRobotProfile("robot"); err != nil || got != p {
		t.Fatalf("failed update changed profile: %+v %v", got, err)
	}
	if err := s.UpdateRobotProfile("robot", func(*RobotProfile) error { return errors.New("stop") }); err == nil {
		t.Fatal("callback error ignored")
	}
	if err := s.CreateRobotProfile("missing", p); err == nil {
		t.Fatal("created a profile without a VM")
	}
	if _, _, err := s.ReadRobotProfile("../robot"); err == nil {
		t.Fatal("accepted invalid VM name")
	}
}
