package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh/carriers"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestOperatorBLEDiscoveryExposesOnlyHints(t *testing.T) {
	oldIdentity, oldDiscovery, oldJSON := operatorBLEDiscoveryIdentity, discoverOperatorBLE, jsonOutput
	t.Cleanup(func() {
		operatorBLEDiscoveryIdentity, discoverOperatorBLE, jsonOutput = oldIdentity, oldDiscovery, oldJSON
	})
	self, _ := meshid.PeerFromPrincipal("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice")
	operatorBLEDiscoveryIdentity = func() (meshid.Device, error) { return self, nil }
	discoverOperatorBLE = func(_ context.Context, got meshid.Device, duration time.Duration) ([]carriers.Candidate, error) {
		if got != self || duration != 20*time.Millisecond {
			t.Fatalf("discovery context changed: %v %v", got, duration)
		}
		return []carriers.Candidate{{Transport: carriers.BLE, Address: "EE48983F-300C-3A22-395B-290A76741CA1", PSM: 129, Hint: [4]byte{1, 2, 3, 4}, NetworkHash: [2]byte{5, 6}, Capabilities: 3}}, nil
	}
	jsonOutput = true
	cmd := newDeviceOperatorMeshDiscoverCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--transport", "ble", "--timeout", "20ms"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Version    int
		HintsOnly  bool
		Tenant     string
		Candidates []operatorBLEHint
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Version != 1 || !result.HintsOnly || result.Tenant != self.Tenant().String() || len(result.Candidates) != 1 || result.Candidates[0].DeviceHint != "01020304" || result.Candidates[0].PSM != 129 || result.Candidates[0].Authenticated {
		t.Fatalf("radio hints became authenticated identity: %s", out.String())
	}
	for _, field := range []string{"routerUUID", "deviceId", "principal"} {
		if strings.Contains(out.String(), field) {
			t.Fatalf("invented identity field %q: %s", field, out.String())
		}
	}
}

func TestOperatorBLEDiscoveryPropagatesScanFailuresAndValidatesBounds(t *testing.T) {
	oldIdentity, oldDiscovery := operatorBLEDiscoveryIdentity, discoverOperatorBLE
	t.Cleanup(func() { operatorBLEDiscoveryIdentity, discoverOperatorBLE = oldIdentity, oldDiscovery })
	self, _ := meshid.PeerFromPrincipal("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice")
	operatorBLEDiscoveryIdentity = func() (meshid.Device, error) { return self, nil }
	want := errors.New("Bluetooth unavailable")
	discoverOperatorBLE = func(context.Context, meshid.Device, time.Duration) ([]carriers.Candidate, error) { return nil, want }
	cmd := newDeviceOperatorMeshDiscoverCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); !errors.Is(err, want) {
		t.Fatalf("scan failure hidden: %v", err)
	}
	operatorBLEDiscoveryIdentity = func() (meshid.Device, error) { t.Fatal("invalid arguments read profile"); return meshid.Device{}, nil }
	for _, args := range [][]string{{"--timeout", "0s"}, {"--timeout", "31s"}, {"--transport", "tcp"}} {
		cmd := newDeviceOperatorMeshDiscoverCmd()
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("invalid discovery arguments accepted: %v", args)
		}
	}
}
