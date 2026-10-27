package localmesh

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadTCPConfigFileSnapshotBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-mesh.json")
	if _, err := ReadTCPConfigFile(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing snapshot must retain its cause: %v", err)
	}
	for _, size := range []int{0, 65536, 65537} {
		want := bytes.Repeat([]byte{'x'}, size)
		if err := os.WriteFile(path, want, 0600); err != nil {
			t.Fatal(err)
		}
		got, err := ReadTCPConfigFile(path)
		if size == 65536 {
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("maximum bounded snapshot: bytes=%d err=%v", len(got), err)
			}
		} else if err == nil {
			t.Fatalf("accepted snapshot size %d", size)
		}
	}
}

// TestTCPConfigRequiresReciprocalConcretePeers: a configured peer is named by
// device UUID, in this device's own tenant, once, with a concrete address.
func TestTCPConfigRequiresReciprocalConcretePeers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-mesh.json")
	self := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	peer := testDevice(t, testPrincipal(testTenantUUID, testDeviceBID))
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"listen":"127.0.0.1:7001","peers":[{"device":"` + peer.UUID().String() + `","address":"127.0.0.1:7002"}]}`, true},
		{`{"listen":"127.0.0.1:7001","peers":[{"device":"` + self.UUID().String() + `","address":"127.0.0.1:7002"}]}`, false},
		{`{"listen":"127.0.0.1:7001","peers":[{"device":"` + peer.UUID().String() + `","address":"0.0.0.0:7002"}]}`, false},
		{`{"listen":"127.0.0.1:7001","peers":[{"device":"` + peer.UUID().String() + `","address":"127.0.0.1:7002"},{"device":"` + peer.UUID().String() + `","address":"127.0.0.1:7003"}]}`, false},
		{`{"listen":"127.0.0.1:7001","peers":[{"device":"2b1c0000-0000-4000-8000-000000000460","address":"127.0.0.1:7002"}]}`, true},
		{`{"listen":"127.0.0.1:7001","peers":[{"device":"orin","address":"127.0.0.1:7002"}]}`, false},
		{`{"listen":"127.0.0.1:7001","peers":[{"asset":2,"address":"127.0.0.1:7002"}]}`, false},
		{`{"listen":"127.0.0.1:7001","peers":[],"unknown":true}`, false},
		{`{"listen":"127.0.0.1:7001","peers":[]} {}`, false},
		{`{"nan":true}`, true},
		{`{"ble":true}`, true},
		{`{"nan":true,"ble":true}`, true},
		{`{"ethernet":true}`, true},
		{`{"infrastructureWifi":true}`, true},
		{`{"ethernet":true,"infrastructureWifi":true}`, true},
		{`{"leafListen":"0.0.0.0:43024"}`, true},
		{`{"leafListen":"[::]:43024","ethernet":true}`, true},
		{`{"leafListen":"0.0.0.0:0"}`, false},
		{`{"leafListen":"missing-port"}`, false},
		{`{"listen":"0.0.0.0:43024","leafListen":"0.0.0.0:43024"}`, false},
		{`{"nan":true,"peers":[{"asset":2,"address":"127.0.0.1:7002"}]}`, false},
		{`{"peers":[]}`, false},
	} {
		if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadTCPConfig(path, self)
		if (err == nil && cfg != nil) != tc.valid {
			t.Errorf("body %s: valid=%v, err=%v", tc.body, tc.valid, err)
		}
	}
	// The parsed peers are devices, in file order, ready to key a link.
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:7001","peers":[{"device":"`+peer.UUID().String()+`","address":"127.0.0.1:7002"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadTCPConfig(path, self)
	if err != nil {
		t.Fatal(err)
	}
	devices := cfg.Devices()
	if len(devices) != 1 || devices[0].Principal() != peer.Principal() {
		t.Fatalf("configured peers are not the configured devices: %+v", devices)
	}
}

func TestTCPConfigRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-mesh.json")
	if err := os.WriteFile(path, make([]byte, 65537), 0600); err != nil {
		t.Fatal(err)
	}
	self := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	if _, err := LoadTCPConfig(path, self); err == nil {
		t.Fatal("oversized configuration accepted")
	}
}
