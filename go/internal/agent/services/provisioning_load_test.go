package services

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestLoadEnrollmentMissingCredentials(t *testing.T) {
	for _, tc := range []struct {
		name        string
		keyFile     string
		missingCert bool
		wantError   string
	}{
		{"missing key", "absent", false, "no usable private key"},
		{"empty key", "empty", false, "no usable private key"},
		{"unreadable key", "directory", false, "no usable private key"},
		{"missing certificate", "present", true, "no certificate"},
		{"intact enrollment", "present", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			state := provisioningState{Enrolled: true, CloudHost: "cloud.example", OrgID: 42, AssetID: 100, CertPEM: "stored-cert", ChainPEM: "stored-chain"}
			if tc.missingCert {
				state.CertPEM = ""
			}
			original, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(dir, "provisioning.json")
			if err := os.WriteFile(statePath, original, 0o600); err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(dir, "device-key.pem")
			const privateKey = "private-key-must-not-appear-in-logs"
			var key []byte
			switch tc.keyFile {
			case "present", "empty":
				if tc.keyFile == "present" {
					key = []byte(privateKey)
				}
				err = os.WriteFile(keyPath, key, 0o600)
			case "directory":
				// A directory produces a read error even when tests run as root.
				err = os.Mkdir(keyPath, 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}

			core, logs := observer.New(zap.ErrorLevel)
			svc := NewProvisioningService(zap.New(core), dir)
			entries := logs.All()
			if tc.wantError == "" {
				if len(entries) != 0 {
					t.Fatalf("healthy enrollment logged errors: %v", entries)
				}
			} else {
				if len(entries) != 1 || !strings.Contains(entries[0].Message, tc.wantError) {
					t.Fatalf("want diagnostic containing %q, got %v", tc.wantError, entries)
				}
				fields := entries[0].ContextMap()
				wantPath := keyPath
				if tc.missingCert {
					wantPath = statePath
				} else if fields["error"] == nil {
					t.Error("key diagnostic lost the read failure")
				}
				if fields["path"] != wantPath {
					t.Errorf("diagnostic path = %v, want %s", fields["path"], wantPath)
				}
				encoded, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(entries[0].Message+string(encoded), privateKey) {
					t.Fatal("private key leaked into diagnostic")
				}
			}

			// Detecting broken enrollment must not erase its identity, replace
			// its key, or silently turn it into a new enrollment.
			host, org, asset, enrolled := svc.ProvisioningInfo()
			if !enrolled || host != state.CloudHost || org != state.OrgID || asset != state.AssetID {
				t.Fatal("stored enrollment identity was discarded")
			}
			cert, chain, loadedKey := svc.ProvisioningCerts()
			if cert != state.CertPEM || chain != state.ChainPEM || !bytes.Equal(loadedKey, key) {
				t.Fatal("stored credentials were changed")
			}
			after, err := os.ReadFile(statePath)
			if err != nil || !bytes.Equal(after, original) {
				t.Fatalf("enrollment record changed: %v", err)
			}
			if tc.keyFile == "absent" {
				if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
					t.Fatalf("missing key was replaced: %v", err)
				}
			}
		})
	}
}

func TestLoadFreshDeviceDoesNotReportBrokenEnrollment(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	svc := NewProvisioningService(zap.New(core), t.TempDir())
	if _, _, _, enrolled := svc.ProvisioningInfo(); enrolled || logs.Len() != 0 {
		t.Fatalf("fresh device treated as broken enrollment: %v", logs.All())
	}
}
