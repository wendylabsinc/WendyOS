//go:build esp32lab

package bleprovider

import (
	"bytes"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNativeESP32DiscoveryAdvertisement(t *testing.T) {
	tool := os.Getenv("WENDY_NATIVE_BLE_ORACLE")
	if tool == "" {
		t.Skip("set WENDY_NATIVE_BLE_ORACLE to the native net_ble_protocol_test binary")
	}
	output, err := exec.Command(tool).CombinedOutput()
	if err != nil {
		t.Fatalf("native codec: %v: %s", err, output)
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(output)))
	if err != nil || len(raw) != 31 {
		t.Fatalf("invalid native AD: %q (%v)", output, err)
	}
	fields, err := parseAdvertisingData(raw, "2558fd76-afc7-466e-9613-6b715296a526")
	if err != nil || !fields.servicePresent || fields.uuidListPresent || fields.namePresent {
		t.Fatalf("native fields: %+v: %v", fields, err)
	}
	got, err := ParseAdvertisement(fields.service, "default")
	if err != nil {
		t.Fatal(err)
	}
	want, err := NewAdvertisement(uuid.MustParse("7cd512eb-eb0c-4f68-b752-148e804e3d11"), "default", 129)
	if err != nil || got != want {
		t.Fatalf("native hint: got %+v want %+v: %v", got, want, err)
	}
	wire, err := want.MarshalBinary()
	if err != nil || !bytes.Equal(wire, fields.service) {
		t.Fatalf("native wire differs from WendyOS: %x != %x (%v)", fields.service, wire, err)
	}
}
