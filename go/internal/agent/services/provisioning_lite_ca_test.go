package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

const (
	liteTestTenant    = "5c9e14bb-49c0-4178-9cc5-493a151f9885"
	liteOtherTenant   = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	liteTestDirectory = "https://acme.pki.example/acme/directory"
)

func TestESTHostFor(t *testing.T) {
	for in, want := range map[string]string{
		"https://acme.dev.pki.wendy.sh/1111/acme/directory": "est.dev.pki.wendy.sh",
		"https://acme.pki.wendy.sh:8444/acme/directory":     "", // explicit port: frontends differ
		"http://acme.pki.wendy.sh/acme/directory":           "",
		"https://user@acme.pki.wendy.sh/acme/directory":     "",
		"https://pki.wendy.sh/acme/directory":               "",
		"":                                                  "",
	} {
		if got := estHostFor(in); got != want {
			t.Errorf("estHostFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// enrolledLiteCAService is an enrolled device in tenant, with fetch standing in
// for the PKI and a controllable clock.
func enrolledLiteCAService(t *testing.T, dir, tenant string, fetch func(string) (string, error)) (*ProvisioningService, *time.Time, *atomic.Int32) {
	t.Helper()
	svc := NewProvisioningService(zap.NewNop(), dir)
	svc.enrolled = true
	svc.principalURI = "spiffe://wendy.sh/tenant/" + tenant + "/device/0cdfb0f4-fd00-43bb-a821-ee28ed0b6cc9"
	svc.acmeDirectoryURL = liteTestDirectory
	now := time.Now()
	calls := new(atomic.Int32)
	svc.liteCA.now = func() time.Time { return now }
	svc.liteCA.fetch = func(_ context.Context, url string) (string, error) {
		calls.Add(1)
		return fetch(url)
	}
	return svc, &now, calls
}

func TestLiteDeviceCAChainFetchesOwnTenantAndCaches(t *testing.T) {
	want := "https://est.pki.example/.well-known/est/" + liteTestTenant + "/cacerts?chain_algorithm=ECDSA_P256&full_chain=true"
	svc, now, calls := enrolledLiteCAService(t, t.TempDir(), liteTestTenant, func(url string) (string, error) {
		if url != want {
			t.Errorf("discovery URL = %q, want %q", url, want)
		}
		return "chain A", nil
	})
	for range 2 {
		if chain, err := svc.LiteDeviceCAChain(context.Background()); err != nil || chain != "chain A" {
			t.Fatalf("chain = %q, err = %v", chain, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fetches within the TTL = %d, want 1", calls.Load())
	}
	*now = now.Add(liteDeviceCAChainTTL)
	if _, err := svc.LiteDeviceCAChain(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitLiteCAFetch(svc)
	if calls.Load() != 2 {
		t.Fatalf("fetches after the TTL = %d, want a refetch", calls.Load())
	}
}

// waitLiteCAFetch waits for a background refresh, so a test can move the clock.
func waitLiteCAFetch(svc *ProvisioningService) {
	svc.liteCA.mu.Lock()
	pending := svc.liteCA.pending
	svc.liteCA.mu.Unlock()
	if pending != nil {
		<-pending
	}
}

// A source's handshake waits on this chain, so an unreachable PKI must not
// stall it while an older chain is at hand.
func TestLiteDeviceCAChainServesTheLastChainWhileRefreshing(t *testing.T) {
	dir := t.TempDir()
	next := make(chan string, 1)
	next <- "chain A"
	fetch := func(string) (string, error) { return <-next, nil }
	svc, now, calls := enrolledLiteCAService(t, dir, liteTestTenant, fetch)
	if chain, err := svc.LiteDeviceCAChain(context.Background()); err != nil || chain != "chain A" {
		t.Fatalf("chain = %q, err = %v", chain, err)
	}
	*now = now.Add(liteDeviceCAChainTTL)
	if chain, err := svc.LiteDeviceCAChain(context.Background()); err != nil || chain != "chain A" {
		t.Fatalf("expired chain = %q, err = %v; want it served during the refresh", chain, err)
	}
	next <- "chain B"
	waitLiteCAFetch(svc)
	if chain, _ := svc.LiteDeviceCAChain(context.Background()); chain != "chain B" || calls.Load() != 2 {
		t.Fatalf("after the refresh: chain = %q, fetches = %d", chain, calls.Load())
	}

	// A restarted agent serves the stored chain at once and refreshes it.
	restarted, _, restartCalls := enrolledLiteCAService(t, dir, liteTestTenant, fetch)
	if chain, err := restarted.LiteDeviceCAChain(context.Background()); err != nil || chain != "chain B" {
		t.Fatalf("after restart: chain = %q, err = %v; want the stored chain without waiting", chain, err)
	}
	next <- "chain B"
	waitLiteCAFetch(restarted)
	if restartCalls.Load() != 1 {
		t.Fatalf("fetches after restart = %d, want a refresh", restartCalls.Load())
	}
}

func TestLiteDeviceCAChainFallsBackToLastGoodChain(t *testing.T) {
	dir := t.TempDir()
	reachable := true
	fetch := func(string) (string, error) {
		if !reachable {
			return "", errors.New("PKI unreachable")
		}
		return "chain A", nil
	}
	svc, now, _ := enrolledLiteCAService(t, dir, liteTestTenant, fetch)
	if _, err := svc.LiteDeviceCAChain(context.Background()); err != nil {
		t.Fatal(err)
	}
	reachable = false
	*now = now.Add(liteDeviceCAChainTTL)
	if chain, err := svc.LiteDeviceCAChain(context.Background()); err != nil || chain != "chain A" {
		t.Fatalf("offline refresh: chain = %q, err = %v; want the last good chain", chain, err)
	}
	waitLiteCAFetch(svc)
	if chain, err := svc.LiteDeviceCAChain(context.Background()); err != nil || chain != "chain A" {
		t.Fatalf("after a failed refresh: chain = %q, err = %v; want the last good chain", chain, err)
	}

	// A restarted agent without PKI reaches the copy kept on disk.
	restarted, _, _ := enrolledLiteCAService(t, dir, liteTestTenant, fetch)
	if chain, err := restarted.LiteDeviceCAChain(context.Background()); err != nil || chain != "chain A" {
		t.Fatalf("after restart: chain = %q, err = %v; want the stored chain", chain, err)
	}
	waitLiteCAFetch(restarted)

	// Another tenant never sees it.
	other, _, _ := enrolledLiteCAService(t, dir, liteOtherTenant, fetch)
	if chain, err := other.LiteDeviceCAChain(context.Background()); err == nil {
		t.Fatalf("other tenant got stored chain %q", chain)
	}
}

func TestLiteDeviceCAChainReusesAFailureBriefly(t *testing.T) {
	svc, now, calls := enrolledLiteCAService(t, t.TempDir(), liteTestTenant, func(string) (string, error) {
		return "", errors.New("PKI unreachable")
	})
	for range 2 {
		if _, err := svc.LiteDeviceCAChain(context.Background()); err == nil || !strings.Contains(err.Error(), "PKI unreachable") {
			t.Fatalf("err = %v, want the fetch failure", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fetches during the retry delay = %d, want 1", calls.Load())
	}
	*now = now.Add(liteDeviceCAChainRetry)
	svc.LiteDeviceCAChain(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("fetches after the retry delay = %d, want 2", calls.Load())
	}
}

func TestLiteDeviceCAChainNeedsEnrollmentAndAuthority(t *testing.T) {
	notEnrolled, _, calls := enrolledLiteCAService(t, t.TempDir(), liteTestTenant, func(string) (string, error) { return "chain A", nil })
	notEnrolled.enrolled = false
	if _, err := notEnrolled.LiteDeviceCAChain(context.Background()); err == nil {
		t.Fatal("a device that is not enrolled fetched Lite CA material")
	}
	noAuthority, _, noAuthorityCalls := enrolledLiteCAService(t, t.TempDir(), liteTestTenant, func(string) (string, error) { return "chain A", nil })
	noAuthority.acmeDirectoryURL = "https://acme.pki.example:8444/acme/directory"
	if _, err := noAuthority.LiteDeviceCAChain(context.Background()); err == nil {
		t.Fatal("derived a discovery host from a directory with an explicit port")
	}
	if calls.Load()+noAuthorityCalls.Load() != 0 {
		t.Fatal("fetched without a usable enrollment")
	}
}

func TestUnenrollmentRemovesStoredLiteCAChain(t *testing.T) {
	dir := t.TempDir()
	svc, _, _ := enrolledLiteCAService(t, dir, liteTestTenant, func(string) (string, error) { return "chain A", nil })
	if _, err := svc.LiteDeviceCAChain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, liteDeviceCAChainDir)); len(entries) != 1 {
		t.Fatalf("stored chains = %d, want 1", len(entries))
	}
	if err := svc.clearCredentialFiles(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, liteDeviceCAChainDir)); !os.IsNotExist(err) {
		t.Fatalf("stored Lite CA chains survived unenrollment: %v", err)
	}
}

func TestLiteDeviceCAChainSharesOneFetch(t *testing.T) {
	release := make(chan struct{})
	svc, _, calls := enrolledLiteCAService(t, t.TempDir(), liteTestTenant, func(string) (string, error) {
		<-release
		return "chain A", nil
	})
	results := make(chan string, 2)
	for range 2 {
		go func() {
			chain, _ := svc.LiteDeviceCAChain(context.Background())
			results <- chain
		}()
	}
	// Both callers are waiting on the one fetch before it is released.
	for deadline := time.Now().Add(5 * time.Second); calls.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	close(release)
	for range 2 {
		if chain := <-results; chain != "chain A" {
			t.Fatalf("chain = %q", chain)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1 shared fetch", calls.Load())
	}
}

func TestLiteDeviceCAChainWaitHonorsCallerContext(t *testing.T) {
	release := make(chan struct{})
	svc, _, _ := enrolledLiteCAService(t, t.TempDir(), liteTestTenant, func(string) (string, error) {
		<-release
		return "chain A", nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := svc.LiteDeviceCAChain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	// The fetch outlives the caller; let it finish before TempDir is removed.
	close(release)
	waitLiteCAFetch(svc)
}
