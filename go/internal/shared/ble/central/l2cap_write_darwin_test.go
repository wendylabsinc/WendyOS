//go:build darwin && cgo

package central

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDarwinL2CAPWritePreservesBackpressuredTLSRecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "l2cap-write-test")
	build := exec.CommandContext(ctx, "xcrun", "clang", "-fobjc-arc", "-fblocks", "-UNDEBUG",
		"-framework", "Foundation", "-framework", "CoreBluetooth",
		"testdata/l2cap_write_test.m", "-o", binary)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile production stream writer: %v\n%s", err, output)
	}
	plain := exec.CommandContext(ctx, binary)
	plain.Env = append(os.Environ(), "WENDY_BLE_TRACE_WRITES=0")
	output, err := plain.CombinedOutput()
	if err != nil {
		t.Fatalf("certificate-sized partial write: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "wendy_ble_write") {
		t.Fatal("write diagnostics must remain opt-in")
	}
	traced := exec.CommandContext(ctx, binary)
	traced.Env = append(os.Environ(), "WENDY_BLE_TRACE_WRITES=1")
	output, err = traced.CombinedOutput()
	if err != nil {
		t.Fatalf("traced certificate-sized partial write: %v\n%s", err, output)
	}
	found, firstPrefix := false, false
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "wendy_ble_write_progress ") {
			var progress map[string]float64
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "wendy_ble_write_progress ")), &progress); err != nil {
				t.Fatalf("parse first prefix: %v: %s", err, line)
			}
			if progress["requested"] == 25646 && progress["accepted"] == 16384 {
				firstPrefix = true
			}
		}
		if !strings.HasPrefix(line, "wendy_ble_write ") {
			continue
		}
		var fields map[string]float64
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "wendy_ble_write ")), &fields); err != nil {
			t.Fatalf("parse metadata: %v: %s", err, line)
		}
		if len(fields) != 8 {
			t.Fatalf("unexpected diagnostic fields: %s", line)
		}
		if fields["last_progress_ms"] < 0 || fields["last_progress_ms"] > fields["elapsed_ms"] {
			t.Fatalf("progress must be within this request: %s", line)
		}
		if fields["requested"] == 25646 && fields["error_code"] == 0 {
			found = true
			if fields["accepted"] != 25646 || fields["write_calls"] != 5 || fields["zero_writes"] != 2 || fields["wait_turns"] < 2 || fields["elapsed_ms"] <= 0 {
				t.Fatalf("partial-write progress counters: %s", line)
			}
		}
	}
	if !found || !firstPrefix {
		t.Fatalf("missing successful certificate-sized write metadata: %s", output)
	}
}

// The net.Conn wrapper must reach the Darwin native setter, including the
// ordinary closed-handle result, rather than silently ignore its deadline.
func TestDarwinL2CAPStreamForwardsWriteDeadline(t *testing.T) {
	stream := NewL2CAPStream(&Connection{})
	if err := stream.SetWriteDeadline(time.Now().Add(time.Second)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write deadline on closed native connection: %v", err)
	}
	if err := stream.SetDeadline(time.Time{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("combined deadline must forward clearing the write deadline: %v", err)
	}
}
