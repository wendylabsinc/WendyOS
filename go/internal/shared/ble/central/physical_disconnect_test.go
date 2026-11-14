//go:build linux || (darwin && cgo)

package central

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"
)

// Explicit human physical acceptance only. No default CI invocation enables it.
// The production Lite provisioning status characteristic is subscribed without
// writing credentials or commands. The operator removes power from the reserved
// peripheral only after both quiet waits are outstanding, then restores it.
func TestPhysicalLiteGATTDisconnect(t *testing.T) {
	path := os.Getenv("WENDY_BLE_PHYSICAL_PLAN")
	if path == "" {
		t.Skip("exact-target human BLE plan required")
	}
	var plan struct{ Platform, Address, DeviceID, Trial, RestoreFile, SuccessFile string }
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(body, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Platform != runtime.GOOS || plan.Address == "" || plan.DeviceID == "" || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(plan.Trial) || !filepath.IsAbs(plan.RestoreFile) || !filepath.IsAbs(plan.SuccessFile) || plan.RestoreFile == plan.SuccessFile {
		t.Fatal("invalid exact-host physical plan")
	}
	for _, path := range []string{plan.RestoreFile, plan.SuccessFile} {
		if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("physical acknowledgment must not exist before this run")
		}
	}
	const infoService = "4E57454E-4459-0002-0000-000000000000"
	const deviceIDChar = "4E57454E-4459-0002-0002-000000000000"
	const provisioningService = "4E57454E-4459-0001-0000-000000000000"
	const statusChar = "4E57454E-4459-0001-0004-000000000000"
	pending := make(map[*Connection]bool)
	open := func() *Connection {
		c, err := Connect(plan.Address, 15)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if pending[c] {
				t.Log("retaining native handle with unjoined physical waiter until owned process teardown")
				return
			}
			c.Close()
		})
		if err = c.DiscoverServices(15); err != nil {
			t.Fatal(err)
		}
		id, err := c.ReadCharacteristic(infoService, deviceIDChar)
		if err != nil || string(id) != plan.DeviceID {
			t.Fatalf("physical public device ID mismatch: %v", err)
		}
		if err = c.Subscribe(provisioningService, statusChar); err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	// A quiet read-only baseline is required. A timeout is expected here; any
	// unsolicited status delivery makes the later outstanding-wait premise false.
	expectedQuiet := "no notification on " + statusChar + " within 1s"
	if runtime.GOOS == "darwin" {
		expectedQuiet = "BLE waiting for notification: timeout"
	}
	if value, err := c.WaitNotification(provisioningService, statusChar, 1); err == nil || err.Error() != expectedQuiet || len(value) != 0 {
		t.Fatal("physical status characteristic is not quiet; stop and reserve the peripheral")
	}
	pending[c] = true
	results := make(chan error, 2)
	started := make(chan struct{}, 2)
	for range 2 {
		go func() {
			started <- struct{}{}
			value, err := c.WaitNotification(provisioningService, statusChar, 20)
			if len(value) != 0 {
				err = fmt.Errorf("unexpected status notification while awaiting physical disconnect")
			}
			results <- err
		}()
	}
	<-started
	<-started
	select {
	case err := <-results:
		// Drain the other finite call before the native handle can be closed.
		select {
		case <-results:
			delete(pending, c)
		case <-time.After(22 * time.Second):
			t.Fatal("physical baseline waiter exceeded its production timeout")
		}
		t.Fatalf("physical waiter returned before power interruption: %v", err)
	case <-time.After(time.Second):
	}
	fmt.Printf("PHYSICAL_BLE_WAIT_READY trial=%s platform=%s; remove only the reserved peripheral power now\n", plan.Trial, plan.Platform)
	deadline := time.Now().Add(15 * time.Second)
	accepted := true
	for range 2 {
		select {
		case err := <-results:
			disconnected := errors.Is(err, ErrGATTDisconnected)
			if runtime.GOOS == "darwin" {
				disconnected = err != nil && err.Error() == "BLE waiting for notification: disconnected"
			}
			if !disconnected || time.Now().After(deadline) {
				t.Errorf("physical pending waiter did not receive prompt disconnect: %v", err)
				accepted = false
			}
		case <-time.After(22 * time.Second):
			t.Fatal("physical waiter exceeded its production timeout")
		}
	}
	delete(pending, c)
	// All callers joined before Close can release the native handle.
	c.Close()
	if !accepted {
		t.FailNow()
	}
	fmt.Printf("PHYSICAL_BLE_DISCONNECTED trial=%s; restore the same peripheral, verify its console identity, then write the nonce acknowledgment\n", plan.Trial)
	restored := false
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		data, err := os.ReadFile(plan.RestoreFile)
		if err == nil {
			if string(data) != plan.Trial+"\n" {
				t.Fatal("restore acknowledgment nonce mismatch")
			}
			restored = true
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !restored {
		t.Fatal("same-peripheral restore acknowledgment timed out")
	}
	replacement := open()
	before, err := replacement.ReadCharacteristic(infoService, deviceIDChar)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, []byte(plan.DeviceID)) {
		t.Fatal("reconnect used another peripheral")
	}
	replacement.Close()
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
	fmt.Printf("PHYSICAL_BLE_GATT_VERIFIED trial=%s platform=%s; two disconnect errors, closed old connection, new public-ID read\n", plan.Trial, plan.Platform)
}
