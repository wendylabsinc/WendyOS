package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
)

func acmeProvisionRequest() *agentpbv2.StartACMEProvisioningRequest {
	return &agentpbv2.StartACMEProvisioningRequest{
		CloudHost: "api.dev.wendy.sh:443", DirectoryUrl: "https://acme.dev.pki.wendy.sh/11111111-1111-4111-8111-111111111111/acme/directory",
		DeviceId: "sim", EabKeyId: "eab-id", EabHmacKey: strings.Repeat("ab", 32),
	}
}

func stubACMEEnroll(t *testing.T, fn func(context.Context, acmeenroll.Config, string, []byte) (string, string, error)) {
	t.Helper()
	old := acmeEnrollDevice
	acmeEnrollDevice = fn
	t.Cleanup(func() { acmeEnrollDevice = old })
}

func TestACMEProvisioningPersistsIdentityWithoutCredentials(t *testing.T) {
	dir := t.TempDir()
	svc := NewProvisioningService(zap.NewNop(), dir)
	req := acmeProvisionRequest()
	var deviceKey []byte
	stubACMEEnroll(t, func(_ context.Context, cfg acmeenroll.Config, path string, key []byte) (string, string, error) {
		if cfg.DeviceID != req.DeviceId || cfg.EABKeyID != req.EabKeyId || cfg.EABHMACKey != req.EabHmacKey {
			t.Fatal("enrollment material changed")
		}
		deviceKey = append([]byte(nil), key...)
		if err := os.WriteFile(path, []byte("account-key"), 0o600); err != nil {
			t.Fatal(err)
		}
		return "leaf", "chain", nil
	})
	called := false
	svc.OnProvisioned = func(leaf, chain string, key []byte) {
		called = true
		_, org, asset, enrolled := svc.ProvisioningInfo() // proves callback is outside mutex
		if !enrolled || org != 0 || asset != 0 || string(key) != string(deviceKey) {
			t.Fatal("invalid callback state")
		}
		key[0] = 0 // callback must receive its own copy
	}
	v2 := NewProvisioningServiceV2(svc)
	resp, err := v2.StartACMEProvisioning(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !called || resp.GetPrincipalUri() != "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/sim" {
		t.Fatal("enrollment did not complete")
	}
	data, err := os.ReadFile(filepath.Join(dir, "provisioning.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{req.EabHmacKey, req.EabKeyId, string(deviceKey), "account-key"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("credentials leaked into provisioning state")
		}
	}
	reloaded := NewProvisioningService(zap.NewNop(), dir)
	info, err := NewProvisioningServiceV2(reloaded).IsProvisioned(context.Background(), &agentpbv2.IsProvisionedRequest{})
	if err != nil || info.GetProvisioned().GetPrincipalUri() != resp.PrincipalUri {
		t.Fatal("identity was not restored")
	}
	_, _, key := reloaded.ProvisioningCerts()
	if string(key) != string(deviceKey) {
		t.Fatal("device key was lost")
	}
	if _, err := reloaded.Unprovision(context.Background(), &agentpb.UnprovisionRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "acme-account-key.pem")); !os.IsNotExist(err) {
		t.Fatal("unprovision retained the ACME account key")
	}
}

func TestACMEProvisioningFailureCanRetry(t *testing.T) {
	svc := NewProvisioningService(zap.NewNop(), t.TempDir())
	attempt := 0
	var firstKey string
	stubACMEEnroll(t, func(_ context.Context, _ acmeenroll.Config, _ string, key []byte) (string, string, error) {
		attempt++
		if attempt == 1 {
			firstKey = string(key)
			return "", "", errors.New("network unavailable")
		}
		if string(key) != firstKey {
			t.Fatal("retry changed the device key")
		}
		return "leaf", "chain", nil
	})
	v2 := NewProvisioningServiceV2(svc)
	if _, err := v2.StartACMEProvisioning(context.Background(), acmeProvisionRequest()); err == nil {
		t.Fatal("expected enrollment failure")
	}
	if _, _, _, enrolled := svc.ProvisioningInfo(); enrolled {
		t.Fatal("failed enrollment was committed")
	}
	if _, err := v2.StartACMEProvisioning(context.Background(), acmeProvisionRequest()); err != nil {
		t.Fatal(err)
	}
}

func TestACMEProvisioningDoesNotSpendEABWithoutDurableDeviceKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "device-key.pem"), 0o700); err != nil {
		t.Fatal(err)
	}
	svc := NewProvisioningService(zap.NewNop(), dir)
	stubACMEEnroll(t, func(context.Context, acmeenroll.Config, string, []byte) (string, string, error) {
		t.Fatal("ACME must not be called when the device key cannot be saved")
		return "", "", nil
	})
	if _, err := NewProvisioningServiceV2(svc).StartACMEProvisioning(context.Background(), acmeProvisionRequest()); err == nil {
		t.Fatal("expected device key persistence failure")
	}
}

func TestACMEProvisioningStateFailureDoesNotCommit(t *testing.T) {
	dir := t.TempDir()
	svc := NewProvisioningService(zap.NewNop(), dir)
	if err := os.Mkdir(filepath.Join(dir, "provisioning.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	stubACMEEnroll(t, func(context.Context, acmeenroll.Config, string, []byte) (string, string, error) {
		return "leaf", "chain", nil
	})
	svc.OnProvisioned = func(string, string, []byte) { t.Fatal("callback must not run before state is persisted") }
	if _, err := NewProvisioningServiceV2(svc).StartACMEProvisioning(context.Background(), acmeProvisionRequest()); err == nil {
		t.Fatal("expected state persistence failure")
	}
	if _, _, _, enrolled := svc.ProvisioningInfo(); enrolled {
		t.Fatal("failed state write marked the agent enrolled")
	}
}
