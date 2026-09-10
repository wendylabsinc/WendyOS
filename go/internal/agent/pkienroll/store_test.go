package pkienroll

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

func TestStoreKeyGeneratedOnceAndReused(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	first, err := s.LoadOrGenerateKey()
	if err != nil {
		t.Fatalf("LoadOrGenerateKey: %v", err)
	}
	second, err := s.LoadOrGenerateKey()
	if err != nil {
		t.Fatalf("LoadOrGenerateKey (second): %v", err)
	}
	if string(first) != string(second) {
		t.Error("the key was regenerated; a renewal must reuse the key the leaf attests")
	}

	// It lives under <configPath>/pki, never beside the Certificate Authority
	// Service triple in the parent directory.
	if got, want := s.KeyPath(), filepath.Join(dir, StoreSubdir, "device-key.pem"); got != want {
		t.Errorf("KeyPath = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "device-key.pem")); !os.IsNotExist(err) {
		t.Error("the pki key was written where the CAS key lives")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(s.KeyPath())
		if err != nil {
			t.Fatalf("stat key: %v", err)
		}
		if got := info.Mode().Perm(); got != keyMode {
			t.Errorf("key mode = %v, want %v", got, os.FileMode(keyMode))
		}
	}
}

func TestStoreLoadReportsNoIdentity(t *testing.T) {
	s := NewStore(t.TempDir())

	if _, err := s.Load(); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("Load on an empty store = %v, want ErrNoIdentity", err)
	}
	if s.Has() {
		t.Error("Has = true on an empty store")
	}

	// A key with no leaf is a half-enrolled store. It must read as absent, so
	// the dialer falls back rather than presenting nothing.
	if _, err := s.LoadOrGenerateKey(); err != nil {
		t.Fatalf("LoadOrGenerateKey: %v", err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("Load with a key but no leaf = %v, want ErrNoIdentity", err)
	}
}

func TestStoreSaveAndLoad(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.LoadOrGenerateKey(); err != nil {
		t.Fatalf("LoadOrGenerateKey: %v", err)
	}

	f := newFakeFrontend(t)
	srv := f.serve()
	defer srv.Close()
	key, err := s.LoadOrGenerateKey()
	if err != nil {
		t.Fatalf("LoadOrGenerateKey: %v", err)
	}
	result, err := Enroll(context.Background(), EnrollRequest{
		CSRFrontendURL:  srv.URL,
		TenantUUID:      testTenant,
		EnrollmentToken: "tok",
		Key:             key,
		CommonName:      "sh/wendy/2/408",
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if err := s.Save(result); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !s.Has() {
		t.Fatal("Has = false after Save")
	}

	material, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if material.LeafPEM != result.LeafPEM {
		t.Error("loaded leaf differs from the one saved")
	}
	if material.ChainPEM != result.ChainPEM {
		t.Error("loaded chain differs from the one saved")
	}
	if string(material.KeyData) != string(key) {
		t.Error("loaded key differs from the one that signed the CSR")
	}
}

func TestStoreRefusesEmptyLeaf(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Save(Result{}); err == nil {
		t.Error("Save accepted an empty leaf")
	}
}

func TestStoreMetadataRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())

	// Absent metadata is the zero value and not an error: a device that was
	// never enrolled has none, and the renewer reads that as nothing to renew.
	meta, err := s.LoadMetadata()
	if err != nil {
		t.Fatalf("LoadMetadata on an empty store: %v", err)
	}
	if meta.TenantUUID != "" {
		t.Errorf("TenantUUID = %q, want empty", meta.TenantUUID)
	}

	want := Metadata{TenantUUID: testTenant, CSREndpoint: "https://" + DevCSRFrontendHost, DeviceName: "sh/wendy/2/408"}
	if err := s.SaveMetadata(want); err != nil {
		t.Fatalf("SaveMetadata: %v", err)
	}
	got, err := s.LoadMetadata()
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if got != want {
		t.Errorf("metadata = %+v, want %+v", got, want)
	}
}

// TestRenewerRenewsAndPersists drives the loop with the clock, the jitter and
// the network all injected, so the schedule under test is the real one but the
// test spends no time and touches no host.
func TestRenewerRenewsAndPersists(t *testing.T) {
	s := NewStore(t.TempDir())
	f := newFakeFrontend(t)
	srv := f.serve()
	defer srv.Close()

	key, err := s.LoadOrGenerateKey()
	if err != nil {
		t.Fatalf("LoadOrGenerateKey: %v", err)
	}
	enrolled, err := Enroll(context.Background(), EnrollRequest{
		CSRFrontendURL:  srv.URL,
		TenantUUID:      testTenant,
		EnrollmentToken: "tok",
		Key:             key,
		CommonName:      "sh/wendy/2/408",
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if err := s.Save(enrolled); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r := NewRenewer(zaptest.NewLogger(t), s, srv.URL, testTenant)
	// Well past the 2/3 point of the issued 30-day leaf, no jitter, and the
	// renewal driven over the test transport.
	r.now = func() time.Time { return enrolled.NotBefore.Add(25 * 24 * time.Hour) }
	r.jitter = func() float64 { return 0 }
	ctx, cancel := context.WithCancel(context.Background())
	// One renewal, then stop. The cancel hangs off the renewal itself and not
	// off the sleep, because a successful pass with a frozen clock schedules
	// the next attempt immediately and never sleeps. Save still runs; the loop
	// then sees the cancelled context and returns.
	inner := r.renew
	r.renew = func(ctx context.Context, req RenewRequest) (Result, error) {
		req.HTTPClient = srv.Client()
		defer cancel()
		return inner(ctx, req)
	}
	r.sleep = func(context.Context, time.Duration) { cancel() }
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("renewer did not return")
	}

	renewedMaterial, err := s.Load()
	if err != nil {
		t.Fatalf("Load after renewal: %v", err)
	}
	if renewedMaterial.LeafPEM == enrolled.LeafPEM {
		t.Error("the store still holds the old leaf; the renewal was not persisted")
	}
	if string(renewedMaterial.KeyData) != string(key) {
		t.Error("the key changed across renewal; it must be reused")
	}
}

// TestRenewerKeepsTheCurrentLeafOnFailure is the property that makes a failed
// renewal cost nothing: the store is written only after a verified issuance.
func TestRenewerKeepsTheCurrentLeafOnFailure(t *testing.T) {
	s := NewStore(t.TempDir())
	f := newFakeFrontend(t)
	srv := f.serve()
	defer srv.Close()

	key, err := s.LoadOrGenerateKey()
	if err != nil {
		t.Fatalf("LoadOrGenerateKey: %v", err)
	}
	enrolled, err := Enroll(context.Background(), EnrollRequest{
		CSRFrontendURL:  srv.URL,
		TenantUUID:      testTenant,
		EnrollmentToken: "tok",
		Key:             key,
		CommonName:      "sh/wendy/2/408",
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if err := s.Save(enrolled); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r := NewRenewer(zaptest.NewLogger(t), s, srv.URL, testTenant)
	r.now = func() time.Time { return enrolled.NotBefore.Add(25 * 24 * time.Hour) }
	r.jitter = func() float64 { return 0 }
	attempts := 0
	r.renew = func(context.Context, RenewRequest) (Result, error) {
		attempts++
		return Result{}, &StatusError{StatusCode: 503}
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.sleep = func(context.Context, time.Duration) { cancel() }
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("renewer did not return")
	}

	if attempts == 0 {
		t.Fatal("no renewal was attempted")
	}
	material, err := s.Load()
	if err != nil {
		t.Fatalf("Load after a failed renewal: %v", err)
	}
	if material.LeafPEM != enrolled.LeafPEM {
		t.Error("a failed renewal replaced the still-valid leaf")
	}
}

func TestRenewerDoesNothingWithoutMetadata(t *testing.T) {
	s := NewStore(t.TempDir())
	r := NewRenewer(zaptest.NewLogger(t), s, "", "")
	done := make(chan struct{})
	go func() { r.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return with no tenant configured")
	}
}
