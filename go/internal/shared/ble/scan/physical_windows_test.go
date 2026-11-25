//go:build windows

package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// This is the actual WinRT watcher process, enabled only by an exact-host human
// plan. Windows central/GATT remains unsupported; this checks discovery only.
func TestPhysicalWinRTScanRestart(t *testing.T) {
	path := os.Getenv("WENDY_BLE_PHYSICAL_PLAN")
	if path == "" {
		t.Skip("exact-target human WinRT plan required")
	}
	var plan struct{ Platform, Address, Trial, PermitFile, SuccessFile string }
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Platform != "windows" || plan.Address == "" || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(plan.Trial) || !filepath.IsAbs(plan.PermitFile) || !filepath.IsAbs(plan.SuccessFile) || plan.PermitFile == plan.SuccessFile {
		t.Fatal("invalid exact-host WinRT plan")
	}
	if _, err := os.Lstat(plan.SuccessFile); !os.IsNotExist(err) {
		t.Fatal("physical completion must not exist before this run")
	}
	// The Python supervisor assigns this process to its retained Job Object
	// before permitting any WinRT child. No watcher can escape an assignment race.
	permitted := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		value, err := os.ReadFile(plan.PermitFile)
		if err == nil {
			if string(value) != plan.Trial+"\n" {
				t.Fatal("supervisor nonce mismatch")
			}
			permitted = true
			break
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !permitted {
		t.Fatal("owned supervisor did not authorize WinRT startup")
	}
	const service = "4E57454E-4459-0002-0000-000000000000"
	for attempt := 1; attempt <= 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		backend, err := newScanner(ctx, []string{service})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		s := backend.(*windowsScanner)
		t.Cleanup(s.Close)
		found := false
		for ctx.Err() == nil {
			devices, err := s.Snapshot(ctx)
			if err != nil {
				s.Close()
				cancel()
				t.Fatal(err)
			}
			for _, device := range devices {
				if device.Address == plan.Address {
					for _, uuid := range device.ServiceUUIDs {
						if CanonicalUUID(uuid) == service {
							found = true
						}
					}
				}
			}
			if found {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		s.Close()
		s.Close()
		cancel()
		// reap() is reached only after both stdout/stderr readers and cmd.Wait join.
		// Merely canceling the context or observing a stale sighting is insufficient.
		drained := false
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			_, err := s.Snapshot(context.Background())
			if err != nil {
				if !strings.HasPrefix(err.Error(), "PowerShell BLE watcher exited") {
					t.Fatal(err)
				}
				drained = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !drained {
			t.Fatal("owned WinRT process/readers did not drain")
		}
		if !found {
			t.Fatal("exact physical Lite advertisement never observed")
		}
		fmt.Printf("PHYSICAL_BLE_WINRT_CYCLE trial=%s cycle=%d; exact advertisement and owned watcher drain\n", plan.Trial, attempt)
	}
	proof, err := os.OpenFile(plan.SuccessFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = proof.WriteString(plan.Trial + "\n"); err != nil {
		proof.Close()
		t.Fatal(err)
	}
	if err = proof.Close(); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("PHYSICAL_BLE_WINRT_VERIFIED trial=%s; two fresh watcher processes, GATT unsupported\n", plan.Trial)
}
