package agentmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSimulationClockTokenRequiresPrivateFile(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "source-read-token")
	token := []byte("test-only-read-token-not-a-signing-credential\n")
	if err := os.WriteFile(path, token, 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0600, 0400, 0644, 0640, 0660} {
		t.Run(mode.String(), func(t *testing.T) {
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			got, err := readCommissionedFile(path, 4096, true)
			if mode&0077 == 0 {
				if err != nil || !bytes.Equal(got, token) {
					t.Fatalf("private token refused or changed: %v", err)
				}
			} else if !errors.Is(err, ErrDenied) || len(got) != 0 {
				t.Fatalf("token exposed outside its owner was accepted: %v", err)
			}
		})
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCommissionedFile(path, 4096); err != nil {
		t.Fatalf("ordinary non-secret commissioning file was unnecessarily restricted: %v", err)
	}
}

func TestSimulationClockConfigurationRefusedBeforeNetwork(t *testing.T) {
	for _, name := range []string{"simulation-without-clock", "two-simulation-clocks", "physical-with-file", "physical-with-source", "source-frame-not-commissioned", "source-token-readable-by-others"} {
		t.Run(name, func(t *testing.T) {
			parent, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m, _ := captureSchemaManager(t)
			f := FileConfig{Policy: m.cfg.Policy, SocketDirectory: filepath.Join(parent, "sockets"), JournalPath: filepath.Join(parent, "world.db")}
			clock := &SimulationClockConfig{URL: "http://127.0.0.1:1", ReadTokenFile: filepath.Join(parent, "read-token"), RunID: "run-a", BootID: "boot-original", PackageSHA256: m.cfg.CaptureSource.PackageSHA256, Frame: f.Policy.Frames[0], AnchorUnixNS: m.cfg.CaptureSource.AnchorUnixNS}
			switch name {
			case "two-simulation-clocks":
				f.SimulationClockFile, f.SimulationClock = filepath.Join(parent, "clock.json"), clock
			case "physical-with-file":
				f.Policy = basePolicy()
				f.SimulationClockFile = filepath.Join(parent, "clock.json")
			case "physical-with-source":
				f.Policy, f.SimulationClock = basePolicy(), clock
			case "source-frame-not-commissioned":
				f.SimulationClock = clock
				clock.Frame.Revision = "survey-other"
			case "source-token-readable-by-others":
				f.SimulationClock = clock
				if err := os.WriteFile(clock.ReadTokenFile, []byte("test-read-token"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(clock.ReadTokenFile, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.Policy.validate(); err != nil {
				t.Fatalf("test requires an otherwise valid policy: %v", err)
			}
			raw, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "world.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			// Cancellation prevents authenticated-time acquisition even if a
			// regression reaches it. These invalid configurations must instead
			// fail the early configuration guard with ErrDenied.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			got, err := Load(ctx, path, nil, nil)
			if !errors.Is(err, ErrDenied) || got != nil {
				t.Fatalf("invalid clock configuration passed early guard: manager=%v, error=%v", got != nil, err)
			}
		})
	}
}
