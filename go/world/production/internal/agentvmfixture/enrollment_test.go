package agentvmfixture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnrollmentBootstrapDoesNotActivateWorld(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, state := filepath.Join(base, "fixture"), filepath.Join(base, "state")
	pub, err := InitializeEnrollment(dir, state)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{pub.ConfigPath, pub.SimulationClockPath, filepath.Join(state, "journal"), filepath.Join(state, "sockets")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("enrollment bootstrap activated World or a source clock", path, err)
		}
	}
	a, err := load(dir)
	if err != nil {
		t.Fatal("enrollment issuer unavailable before World commissioning", err)
	}
	_, chain := enroll(t, a)
	if len(chain) != 3 {
		t.Fatal("normal ACME enrollment failed without active World")
	}
	for _, path := range []string{pub.ConfigPath, pub.SimulationClockPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("enrollment installed a World configuration", err)
		}
	}
	if _, err := InitializeEnrollment(dir, state); err == nil {
		t.Fatal("enrollment bootstrap reused issuer material")
	}
	if _, err := Initialize(dir, state); err == nil {
		t.Fatal("scalar initialization replaced a prior bootstrap")
	}
}
