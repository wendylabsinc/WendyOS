package qdl

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHardwareEDLHandshake talks to a real board in EDL mode. It reads the chip
// id, uploads the Firehose programmer, negotiates a session and resets the
// device. It programs nothing, so storage is untouched.
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
	if len(devices) != 1 {
		t.Fatalf("expected exactly one EDL device, found %d; disconnect other EDL devices", len(devices))
	}

	if expected := os.Getenv("WENDY_HW_EDL_INSTANCE"); expected != "" && !strings.EqualFold(devices[0].Instance, expected) {
		t.Fatalf("selected device %q differs from authorized instance %q", devices[0].Instance, expected)
	}

	prog, err := os.ReadFile(filepath.Join(bundle, "prog_firehose_ddr.elf"))
	if err != nil {
		t.Fatalf("reading programmer: %v", err)
	}
	if expected := os.Getenv("WENDY_HW_EDL_PROGRAMMER_SHA256"); expected != "" && fmt.Sprintf("%x", sha256.Sum256(prog)) != expected {
		t.Fatal("programmer bytes differ from the authorized checksum")
	}

	conn, err := Open(devices[0])
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close() //nolint:errcheck
	t.Logf("claimed %s", conn.Info())
	// Use the install's actual sequence on one connection. SWITCH_MODE must
	// leave the live Sahara session ready for the programmer image transfer.
	id, err := ReadChipID(conn)
	if err != nil {
		t.Fatalf("ReadChipID: %v", err)
	}
	t.Logf("chip id: msm_id %#x", id.MsmID)
	if expected := os.Getenv("WENDY_HW_EDL_EXPECT_MSM"); expected != "" {
		want, err := strconv.ParseUint(expected, 0, 32)
		if err != nil || id.MsmID != uint32(want) {
			t.Fatalf("chip ID %#x differs from expected %s: %v", id.MsmID, expected, err)
		}
	}

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
	t.Log("reset requested; set the EDL DIP switch OFF and power-cycle to boot normally")
}
