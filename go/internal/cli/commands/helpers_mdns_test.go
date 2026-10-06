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
			// Trailing dot and mixed case must still match the saved default.
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

func TestDefaultDeviceUnreachableError(t *testing.T) {
	cause := errors.New("connection refused")
	err := defaultDeviceUnreachableError("wendyos-workshop-16.local", cause)
	if !errors.Is(err, cause) {
		t.Fatal("expected wrapped error to unwrap to the underlying cause")
	}
	msg := err.Error()
	for _, want := range []string{"wendyos-workshop-16.local", "set but could not be reached", "get-default"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	// A ".local" target carries the mDNS troubleshooting hint.
	if !strings.Contains(msg, "5353") {
		t.Errorf("expected mDNS hint for a .local default, got %q", msg)
	}

	// A non-".local" target must not carry the mDNS hint.
	if msg := defaultDeviceUnreachableError("192.168.1.50", cause).Error(); strings.Contains(msg, "5353") {
		t.Errorf("did not expect mDNS hint for an IP default, got %q", msg)
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

// The text must stay byte-identical to what fmt.Errorf used to build, and the
// advice must now also be available as structured next steps.
func TestDefaultDeviceUnreachableErrorCarriesNextSteps(t *testing.T) {
	cause := errors.New("connection refused")
	err := defaultDeviceUnreachableError("wendyos-workshop-16.local", cause)
	confirm := "Confirm it with 'wendy device get-default'; change it with 'wendy device set-default' or clear it with 'wendy device unset-default'."
	mdns := "Resolving a .local name needs mDNS: ensure avahi-daemon is running and UDP 5353 isn't firewalled (e.g. 'sudo ufw allow 5353/udp'), or connect by IP."
	want := `default device "wendyos-workshop-16.local" is set but could not be reached: connection refused` +
		"\n  " + confirm + "\n  " + mdns
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	if got := NextSteps(err); len(got) != 2 || got[0] != confirm || got[1] != mdns {
		t.Errorf("NextSteps = %q, want [confirm, mdns]", got)
	}
	if got := NextSteps(defaultDeviceUnreachableError("192.168.1.50", cause)); len(got) != 1 || got[0] != confirm {
		t.Errorf("NextSteps for an IP default = %q, want only the confirm step", got)
	}
}
