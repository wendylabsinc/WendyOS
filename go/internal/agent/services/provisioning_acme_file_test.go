package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func bakedACMEFixture(t *testing.T) string {
	t.Helper()
	req := acmeProvisionRequest()
	data, err := json.Marshal(map[string]string{
		"directoryURL": req.DirectoryUrl, "deviceID": req.DeviceId,
		"eabKeyID": req.EabKeyId, "eabHMACKey": req.EabHmacKey, "cloudHost": req.CloudHost,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "acme-enrollment.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestACMEFirstBootCompletesByDefaultAndConsumesHandoff(t *testing.T) {
	path := bakedACMEFixture(t)
	dir := t.TempDir()
	svc := NewProvisioningService(zap.NewNop(), dir)
	called := false
	svc.OnProvisioned = func(string, string, []byte) { called = true }
	attempts := 0
	stubACMEEnroll(t, func(_ context.Context, cfg acmeenroll.Config, _ string, _ []byte) (string, string, error) {
		attempts++
		if cfg.DeviceID != acmeProvisionRequest().DeviceId {
			t.Fatal("baked PKI identity changed")
		}
		if _, err := os.Stat(filepath.Join(dir, "acme-first-boot-attempt.json")); err != nil {
			t.Fatal("attempt was not recorded before redemption")
		}
		return "leaf", "chain", nil
	})
	v2 := NewProvisioningServiceV2(svc)
	v2.ApplyACMEEnrollmentFile(context.Background(), path)
	if !called || attempts != 1 {
		t.Fatal("first boot did not complete provisioning")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("successful handoff was not consumed")
	}
	state, err := os.ReadFile(filepath.Join(dir, "provisioning.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state), "/device/sim") || strings.Contains(string(state), acmeProvisionRequest().EabHmacKey) {
		t.Fatal("identity was lost or secret leaked into state")
	}
	v2.ApplyACMEEnrollmentFile(context.Background(), path)
	if attempts != 1 {
		t.Fatal("successful first boot was repeated")
	}
}

func TestACMEFirstBootFailurePreservesMaterialAndBlocksRestartRetry(t *testing.T) {
	path := bakedACMEFixture(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	core, logs := observer.New(zap.WarnLevel)
	attempts := 0
	stubACMEEnroll(t, func(_ context.Context, _ acmeenroll.Config, account string, _ []byte) (string, string, error) {
		attempts++
		if err := os.WriteFile(account, []byte("fixture-account-key"), 0o600); err != nil {
			t.Fatal(err)
		}
		return "", "", errors.New("upstream included " + acmeProvisionRequest().EabHmacKey)
	})
	NewProvisioningServiceV2(NewProvisioningService(zap.New(core), dir)).ApplyACMEEnrollmentFile(context.Background(), path)
	// A fresh service simulates process restart, not merely a second call.
	NewProvisioningServiceV2(NewProvisioningService(zap.New(core), dir)).ApplyACMEEnrollmentFile(context.Background(), path)
	if attempts != 1 {
		t.Fatal("uncertain issuance automatically retried")
	}
	retained, err := os.ReadFile(path)
	if err != nil || string(retained) != string(original) {
		t.Fatal("failed enrollment lost its credential")
	}
	for _, name := range []string{"device-key.pem", "acme-account-key.pem", "acme-first-boot-attempt.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("recovery material lost: %s", name)
		}
	}
	for _, entry := range logs.All() {
		data, _ := json.Marshal(entry.ContextMap())
		if strings.Contains(entry.Message+string(data), acmeProvisionRequest().EabHmacKey) {
			t.Fatal("upstream credential leaked to logs")
		}
	}
}

func TestACMEFirstBootRefusalsPreserveHandoff(t *testing.T) {
	for _, reason := range []string{"invalid", "already provisioned", "legacy conflict", "marker unavailable", "previous attempt"} {
		t.Run(reason, func(t *testing.T) {
			path := bakedACMEFixture(t)
			dir := t.TempDir()
			svc := NewProvisioningService(zap.NewNop(), dir)
			stubACMEEnroll(t, func(context.Context, acmeenroll.Config, string, []byte) (string, string, error) {
				t.Fatal("refused handoff spent a credential")
				return "", "", nil
			})
			switch reason {
			case "invalid":
				if err := os.WriteFile(path, []byte(`{"eabHMACKey":"private-fixture"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "already provisioned":
				svc.enrolled = true
			case "legacy conflict":
				if err := os.WriteFile(filepath.Join(dir, enrollmentFileName), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "marker unavailable":
				svc.configPath = filepath.Join(dir, "not-a-directory")
				if err := os.WriteFile(svc.configPath, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "previous attempt":
				if err := os.WriteFile(filepath.Join(dir, "acme-first-boot-attempt.json"), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			NewProvisioningServiceV2(svc).ApplyACMEEnrollmentFile(context.Background(), path)
			if _, err := os.Stat(path); err != nil {
				t.Fatal("refusal removed staged credential")
			}
			if _, err := os.Stat(filepath.Join(dir, "device-key.pem")); !os.IsNotExist(err) {
				t.Fatal("refusal generated a device key")
			}
		})
	}
}
