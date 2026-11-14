//go:build darwin && cgo

package central

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDarwinDisconnectPendingOperations(t *testing.T) {
	source, err := os.ReadFile("ble_darwin.m")
	if err != nil {
		t.Fatal(err)
	}
	for _, control := range []string{"normal", "missing-broadcast", "missing-read-wakeup"} {
		t.Run(control, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "testdata"), 0700); err != nil {
				t.Fatal(err)
			}
			body := string(source)
			if control != "normal" {
				before, after := "", ""
				if control == "missing-broadcast" {
					before = "[self.notifyCond broadcast];\n    [self.notifyCond unlock];\n}\n\n// ── CBPeripheralDelegate"
					after = "/* regression control: omit disconnect broadcast */\n    [self.notifyCond unlock];\n}\n\n// ── CBPeripheralDelegate"
				} else {
					before = "dispatch_semaphore_signal(self.readSema);\n    dispatch_semaphore_signal(self.writeSema);"
					after = "/* regression control: omit read wakeup */\n    dispatch_semaphore_signal(self.writeSema);"
				}
				if strings.Count(body, before) != 1 {
					t.Fatal("native disconnect control source site changed")
				}
				body = strings.Replace(body, before, after, 1)
			}
			if err := os.WriteFile(filepath.Join(root, "ble_darwin.m"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"ble_darwin.h", "testdata/notification_bounds_test.m", "testdata/disconnect_waiters_test.m"} {
				data, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			binary := filepath.Join(root, "disconnect-waiters-test")
			guardian, err := filepath.Abs("../../../../../.github/scripts/run-owned-media-test.py")
			if err != nil {
				t.Fatal(err)
			}
			// The guardian joins the whole compiler process group before returning,
			// so xcrun/clang descendants cannot outlive these temporary sources.
			cmd := exec.Command("python3", guardian, "--timeout", "10", "--", "xcrun", "clang", "-fobjc-arc", "-fblocks", "-UNDEBUG", "-framework", "Foundation", "-framework", "CoreBluetooth", filepath.Join(root, "testdata/disconnect_waiters_test.m"), "-o", binary)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("compile native disconnect fixture: %v\n%s", err, output)
			}
			cmd = exec.CommandContext(ctx, binary)
			cmd.WaitDelay = time.Second
			output, err := cmd.CombinedOutput()
			if control == "normal" {
				if err != nil || !strings.Contains(string(output), "native disconnect waiters verified:") {
					t.Fatalf("native disconnect fixture: %v\n%s", err, output)
				}
			} else {
				expected := "native disconnect failed to wake all notification waiters"
				if control == "missing-read-wakeup" {
					expected = "native disconnect failed to wake read"
				}
				if err == nil || ctx.Err() != nil || !strings.Contains(string(output), expected) {
					t.Fatalf("native disconnect control missed intended failure: %v\n%s", err, output)
				}
			}
			t.Logf("native disconnect %s: %s", control, output)
		})
	}
}
