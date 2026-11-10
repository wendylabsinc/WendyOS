package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestNormalizeMDNSHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"wendy-thor.local", "wendy-thor"},
		{"wendy-thor.local.", "wendy-thor"},
		{"Wendy-Thor.LOCAL", "wendy-thor"},
		{"  wendy-thor.local  ", "wendy-thor"},
		{"wendy-thor", "wendy-thor"},
	}
	for _, c := range cases {
		if got := normalizeMDNSHost(c.in); got != c.want {
			t.Errorf("normalizeMDNSHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveMDNSHost_MatchesByHostname(t *testing.T) {
	orig := lanBrowseFn
	defer func() { lanBrowseFn = orig }()
	lanBrowseFn = func(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
		return []models.LANDevice{
			{Hostname: "other.local", IPAddress: "192.168.1.10"},
			// Trailing dot and mixed case must still match the requested host.
			{Hostname: "Wendy-Thor.local.", IPAddress: "192.168.1.50"},
		}, nil
	}

	got := resolveMDNSHost(context.Background(), "wendy-thor.local")
	if got != "192.168.1.50" {
		t.Fatalf("resolveMDNSHost = %q, want %q", got, "192.168.1.50")
	}
}

func TestResolveMDNSHost_NonLocalSkipsBrowse(t *testing.T) {
	orig := lanBrowseFn
	defer func() { lanBrowseFn = orig }()
	called := false
	lanBrowseFn = func(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
		called = true
		return nil, nil
	}

	if got := resolveMDNSHost(context.Background(), "example.com"); got != "" {
		t.Fatalf("resolveMDNSHost(non-.local) = %q, want empty", got)
	}
	if called {
		t.Fatal("expected no mDNS browse for a non-.local host")
	}
}

func TestResolveMDNSHost_NoMatchReturnsEmpty(t *testing.T) {
	orig := lanBrowseFn
	defer func() { lanBrowseFn = orig }()
	lanBrowseFn = func(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
		return []models.LANDevice{{Hostname: "other.local", IPAddress: "192.168.1.10"}}, nil
	}

	if got := resolveMDNSHost(context.Background(), "wendy-thor.local"); got != "" {
		t.Fatalf("resolveMDNSHost(no match) = %q, want empty", got)
	}
}

func TestResolveAddrOnce_LiteralIPUnchanged(t *testing.T) {
	if got := resolveAddrOnce(context.Background(), "192.168.1.50:50051"); got != "192.168.1.50:50051" {
		t.Fatalf("resolveAddrOnce(IP) = %q, want unchanged", got)
	}
}

func TestResolveAddrOnce_PrefersIPv4FromOSResolver(t *testing.T) {
	orig := osLookupHostFn
	defer func() { osLookupHostFn = orig }()
	osLookupHostFn = func(ctx context.Context, host string) ([]string, error) {
		return []string{"fe80::1", "192.168.1.50"}, nil
	}

	if got := resolveAddrOnce(context.Background(), "wendy-thor.local:50051"); got != "192.168.1.50:50051" {
		t.Fatalf("resolveAddrOnce = %q, want %q", got, "192.168.1.50:50051")
	}
}

// When the OS resolver can't see the mDNS ".local" name (the Windows/Linux
// failure mode behind issue #1155), resolveAddrOnce must fall back to an mDNS
// browse rather than returning the unresolvable name.
func TestResolveAddrOnce_FallsBackToMDNSWhenOSResolverFails(t *testing.T) {
	origLookup := osLookupHostFn
	origBrowse := lanBrowseFn
	defer func() {
		osLookupHostFn = origLookup
		lanBrowseFn = origBrowse
	}()

	osLookupHostFn = func(ctx context.Context, host string) ([]string, error) {
		return nil, errors.New("no such host")
	}
	lanBrowseFn = func(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
		return []models.LANDevice{{Hostname: "wendy-thor.local", IPAddress: "192.168.1.50"}}, nil
	}

	if got := resolveAddrOnce(context.Background(), "wendy-thor.local:50051"); got != "192.168.1.50:50051" {
		t.Fatalf("resolveAddrOnce = %q, want %q", got, "192.168.1.50:50051")
	}
}

func TestMDNSBrowseTimeoutValue(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", mdnsBrowseTimeout},
		{"8s", 8 * time.Second},
		{"500ms", mdnsBrowseTimeout}, // below floor → default
		{"60s", mdnsBrowseTimeout},   // above ceiling → default
		{"garbage", mdnsBrowseTimeout},
	}
	for _, c := range cases {
		t.Setenv("WENDY_MDNS_TIMEOUT", c.env)
		if got := mdnsBrowseTimeoutValue(); got != c.want {
			t.Errorf("WENDY_MDNS_TIMEOUT=%q → %v, want %v", c.env, got, c.want)
		}
	}
}

func TestMDNSLocalHint(t *testing.T) {
	if got := mdnsLocalHint("wendy-thor.local"); !strings.Contains(got, "5353") || !strings.Contains(got, "mDNS") {
		t.Errorf("mdnsLocalHint(.local) = %q, want mDNS/5353 guidance", got)
	}
	for _, h := range []string{"example.com", "192.168.1.50", "wendy-thor"} {
		if got := mdnsLocalHint(h); got != "" {
			t.Errorf("mdnsLocalHint(%q) = %q, want empty", h, got)
		}
	}
}

func TestResolveHostMDNSFallback(t *testing.T) {
	origLookup := osLookupHostFn
	origBrowse := lanBrowseFn
	defer func() {
		osLookupHostFn = origLookup
		lanBrowseFn = origBrowse
	}()

	// IP literal passes through untouched (no resolver calls).
	osLookupHostFn = func(context.Context, string) ([]string, error) {
		t.Fatal("OS resolver should not be called for an IP literal")
		return nil, nil
	}
	if got := resolveHostMDNSFallback(context.Background(), "192.168.1.50"); got != "192.168.1.50" {
		t.Fatalf("resolveHostMDNSFallback(IP) = %q, want unchanged", got)
	}

	// OS resolver result prefers IPv4 over IPv6.
	osLookupHostFn = func(context.Context, string) ([]string, error) {
		return []string{"fe80::1", "192.168.1.50"}, nil
	}
	if got := resolveHostMDNSFallback(context.Background(), "wendy-thor.local"); got != "192.168.1.50" {
		t.Fatalf("resolveHostMDNSFallback(OS) = %q, want 192.168.1.50", got)
	}

	// OS resolver failure on a ".local" name falls back to the mDNS browse.
	osLookupHostFn = func(context.Context, string) ([]string, error) {
		return nil, errors.New("no such host")
	}
	lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) {
		return []models.LANDevice{{Hostname: "wendy-thor.local", IPAddress: "10.0.0.7"}}, nil
	}
	if got := resolveHostMDNSFallback(context.Background(), "wendy-thor.local"); got != "10.0.0.7" {
		t.Fatalf("resolveHostMDNSFallback(mDNS) = %q, want 10.0.0.7", got)
	}

	// OS failure on a non-".local" name yields "" (no browse attempted).
	lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) {
		t.Fatal("mDNS browse should not run for a non-.local host")
		return nil, nil
	}
	if got := resolveHostMDNSFallback(context.Background(), "example.com"); got != "" {
		t.Fatalf("resolveHostMDNSFallback(non-.local fail) = %q, want empty", got)
	}
}
