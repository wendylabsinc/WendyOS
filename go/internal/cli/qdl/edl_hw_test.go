package qdl

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestHardwareEDLHandshake talks to a real board in EDL mode. It uploads the
// Firehose programmer, negotiates a session and resets the device. It programs
// nothing, so storage is untouched.
//
// Run with: WENDY_HW_EDL_BUNDLE=<dir> go test -run TestHardwareEDLHandshake -v
func TestHardwareEDLHandshake(t *testing.T) {
	bundle := os.Getenv("WENDY_HW_EDL_BUNDLE")
	if bundle == "" {
		t.Skip("set WENDY_HW_EDL_BUNDLE to a flashed bundle directory")
	}

	devices, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	t.Logf("EDL devices: %v", devices)
	if len(devices) == 0 {
		t.Fatal("no device in EDL mode")
	}

	prog, err := os.ReadFile(filepath.Join(bundle, "prog_firehose_ddr.elf"))
	if err != nil {
		t.Fatalf("reading programmer: %v", err)
	}

	conn, err := Open(DeviceInfo{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close() //nolint:errcheck
	t.Logf("claimed %s", conn.Info())

	start := time.Now()
	err = UploadProgrammer(conn, "prog_firehose_ddr.elf", prog, func(sent, total int64) {
		if sent == total {
			t.Logf("sahara: sent %d/%d bytes in %s", sent, total, time.Since(start).Round(time.Millisecond))
		}
	})
	if err != nil {
		t.Fatalf("UploadProgrammer: %v", err)
	}

	s := NewSession(conn, func(l string) { t.Logf("device: %s", l) })
	if err := s.Configure(StorageUFS); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	t.Logf("negotiated payload size: %d bytes", s.maxPayload)

	if err := s.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	t.Log("reset requested; the board should boot normally")
}
