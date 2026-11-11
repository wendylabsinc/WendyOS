package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func bakedACMEFixture(t *testing.T) string {
	t.Helper()
	oldWait := waitForFirstBootACME
	t.Cleanup(func() { waitForFirstBootACME = oldWait })
	waitForFirstBootACME = func(context.Context, string, *zap.Logger) error { return nil }
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

func TestACMEFirstBootWaitsBeforeRecordingAttempt(t *testing.T) {
	for _, online := range []bool{false, true} {
		t.Run(fmt.Sprint(online), func(t *testing.T) {
			path := bakedACMEFixture(t)
			dir := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waited := false
			waitForFirstBootACME = func(ctx context.Context, directory string, _ *zap.Logger) error {
				waited = true
				if directory != acmeProvisionRequest().DirectoryUrl {
					t.Fatal("wrong connectivity target")
				}
				for _, name := range []string{"acme-first-boot-attempt.json", "device-key.pem", "acme-account-key.pem"} {
					if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
						t.Fatalf("offline boot created %s", name)
					}
				}
				if !online {
					cancel()
					return ctx.Err()
				}
				return nil
			}
			attempts := 0
			stubACMEEnroll(t, func(context.Context, acmeenroll.Config, string, []byte) (string, string, error) {
				attempts++
				if !waited {
					t.Fatal("enrollment started before connectivity")
				}
				return "leaf", "chain", nil
			})
			NewProvisioningServiceV2(NewProvisioningService(zap.NewNop(), dir)).ApplyACMEEnrollmentFile(ctx, path)
			if !waited {
				t.Fatal("first boot did not wait for connectivity")
			}
			if online && attempts != 1 {
				t.Fatal("enrollment did not start after connectivity")
			}
			if !online {
				if attempts != 0 {
					t.Fatal("offline boot attempted enrollment")
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("offline boot lost its handoff")
				}
				// A later boot can still complete the first enrollment attempt.
				waitForFirstBootACME = func(context.Context, string, *zap.Logger) error { return nil }
				NewProvisioningServiceV2(NewProvisioningService(zap.NewNop(), dir)).ApplyACMEEnrollmentFile(context.Background(), path)
				if attempts != 1 {
					t.Fatal("offline boot blocked later enrollment")
				}
			}
		})
	}
}

func TestWaitForACMEDirectoryRetriesOnlyPublicDiscovery(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/directory" || r.ContentLength > 0 {
			t.Error("connectivity probe attempted credential redemption")
		}
		if requests == 1 {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"newNonce":"/nonce","newAccount":"/account","newOrder":"/order"}`)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := waitForACMEDirectory(ctx, server.URL+"/directory", time.Millisecond, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if requests < 2 {
		t.Fatal("connectivity failure was not retried")
	}
}

func TestWaitForACMEDirectoryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	defer cancel()
	if err := waitForACMEDirectory(ctx, server.URL, time.Hour, zap.NewNop()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

// A non-network failure is logged with its reason, once per distinct reason,
// so a wrong directory URL or unset clock does not read as "no network".
func TestWaitForACMEDirectoryLogsFailureReasons(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch {
		case requests <= 3:
			http.NotFound(w, r)
		case requests == 4:
			http.Error(w, "captive portal", http.StatusForbidden)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"newNonce":"/nonce","newAccount":"/account","newOrder":"/order"}`)
		}
	}))
	defer server.Close()
	core, logs := observer.New(zap.InfoLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := waitForACMEDirectory(ctx, server.URL+"/directory", time.Millisecond, zap.New(core)); err != nil {
		t.Fatal(err)
	}
	retries := logs.FilterMessage("ACME directory not reachable yet; retrying").All()
	if len(retries) != 2 {
		t.Fatalf("logged %d retry lines for two distinct reasons", len(retries))
	}
	for i, want := range []string{"404", "403"} {
		if reason, _ := retries[i].ContextMap()["error"].(string); !strings.Contains(reason, want) {
			t.Fatalf("retry %d reason %q does not name %s", i, reason, want)
		}
	}
	if logs.FilterMessage("ACME directory reachable; starting first-boot enrollment").Len() != 1 {
		t.Fatal("recovery after failed probes was not logged")
	}
}

// An enrollment that completes while the handoff waits leaves the handoff
// unspent and says so.
func TestACMEFirstBootStandsDownWhenEnrolledWhileWaiting(t *testing.T) {
	path := bakedACMEFixture(t)
	dir := t.TempDir()
	core, logs := observer.New(zap.WarnLevel)
	svc := NewProvisioningService(zap.New(core), dir)
	waitForFirstBootACME = func(context.Context, string, *zap.Logger) error {
		svc.mu.Lock()
		svc.enrolled = true
		svc.mu.Unlock()
		return nil
	}
	stubACMEEnroll(t, func(context.Context, acmeenroll.Config, string, []byte) (string, string, error) {
		t.Fatal("handoff spent after another enrollment completed")
		return "", "", nil
	})
	NewProvisioningServiceV2(svc).ApplyACMEEnrollmentFile(context.Background(), path)
	if _, err := os.Stat(filepath.Join(dir, "acme-first-boot-attempt.json")); !os.IsNotExist(err) {
		t.Fatal("unused handoff recorded an attempt")
	}
	if logs.FilterMessageSnippet("enrolled by another request").Len() != 1 {
		t.Fatalf("stand-down was not logged: %v", logs.All())
	}
}
