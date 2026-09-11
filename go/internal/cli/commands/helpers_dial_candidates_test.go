package commands

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// TestLanDialCandidatesIncludesEverySightedAddress proves the picker connect
// path supplies the dial ladder with every interface a multi-homed device was
// seen at (models.LANDevice.Addresses), not just the primary — the discovery
// half of the fix. The ladder end is proven by TestLadderWalksEveryCandidateAddress.
func TestLanDialCandidatesIncludesEverySightedAddress(t *testing.T) {
	dev := models.LANDevice{
		DisplayName: "orin",
		Hostname:    "orin.local",
		IPAddress:   "192.168.1.69",
		// A WiFi IPv4 (primary) plus a USB link-local IPv4 the CLI may be the
		// only path actually reachable.
		Addresses: []string{"192.168.1.69", "169.254.1.2"},
		Port:      50051,
	}

	cands := lanDialCandidates(dev)

	wantAddrs := []string{"192.168.1.69", "169.254.1.2"}
	for _, want := range wantAddrs {
		found := false
		for _, c := range cands {
			host, _, err := net.SplitHostPort(c)
			if err == nil && host == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("address %s was dropped from the dial candidates %v", want, cands)
		}
	}
	// A single-address device must still work (fall back to the primary path).
	single := lanDialCandidates(models.LANDevice{Hostname: "n.local", IPAddress: "10.0.0.5", Port: 50051})
	if len(single) == 0 {
		t.Fatal("single-address device produced no dial candidates")
	}
}

// TestPinnedUnreachableDoesNotBlameThePin is the bug report, in test form.
//
// A pinned device on a rotating-DHCP network moves to a new address. Every mTLS
// rung against the address we happen to hold fails with a plain transport error
// — nothing answered, no certificate was ever seen, no identity was ever
// compared. The pin is intact and the device is fine; only the routing is
// stale.
//
// The message the user got in that situation told them their device's identity
// was suspect and invited them to run `wendy device unpin`, which destroys a
// valid trust binding to work around a reachability problem. "No authenticated
// endpoint answered" and "the wrong device answered" are different facts and
// must not share a message.
func TestPinnedUnreachableDoesNotBlameThePin(t *testing.T) {
	setTempConfig(t, &config.Config{DevicePins: map[string]config.DevicePin{
		"orin": {OrgID: 7, CloudGRPC: "grpc.wendy.dev:443", AssetID: "42", Source: config.PinSourceLAN},
	}})
	setPinCache(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	target := newDialTarget("orin.local", deadAgentAddr(t))
	conn, _, err := dialAgentLadderWithCerts(ctx, target, []config.CertificateInfo{selfSignedCLICert(t, 7)})
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("ladder returned no error for a pinned host whose mTLS rungs all failed")
	}

	msg := err.Error()
	if strings.Contains(msg, "device unpin") {
		t.Errorf("nothing answered, so no identity was ever compared, yet the error tells the user to unpin:\n  %s", msg)
	}
	if !strings.Contains(msg, "pin is intact") {
		t.Errorf("error must say the pin is intact so the user does not reach for unpin anyway:\n  %s", msg)
	}
	if !strings.Contains(msg, "reachability") {
		t.Errorf("error must name reachability as the likely cause:\n  %s", msg)
	}

	// Typed, so the two facts are distinguishable without matching wording.
	if !errors.Is(err, errNoAuthenticatedEndpoint) {
		t.Errorf("err = %v does not read as errNoAuthenticatedEndpoint", err)
	}
	if errors.Is(err, errDeviceIdentityRefused) {
		t.Errorf("err = %v reads as an identity refusal, but no identity was ever compared", err)
	}
	// ...but it must still forbid the unauthenticated Bluetooth fallback: this
	// host is PINNED, so we know it authenticates, and a BLE peer advertising
	// its name proves nothing (see blocksUnauthenticatedFallback).
	if !blocksUnauthenticatedFallback(err) {
		t.Errorf("err = %v would let a pinned device be reached over a transport that enforces nothing", err)
	}
}

// TestLadderWalksEveryCandidateAddress is the other half of the bug: resolution
// legitimately returns several addresses and the ladder used to try exactly one.
//
// Every candidate here is dead, so the assertion is on the ladder's own account
// of where it looked — which is also what the user is shown. A ladder that stops
// at the first address cannot name the rest.
func TestLadderWalksEveryCandidateAddress(t *testing.T) {
	setTempConfig(t, &config.Config{DevicePins: map[string]config.DevicePin{
		"orin": {OrgID: 7, CloudGRPC: "grpc.wendy.dev:443", AssetID: "42", Source: config.PinSourceLAN},
	}})
	setPinCache(t)

	// The report's own shape: one IPv4 address plus two ULA IPv6 addresses the
	// device is not actually reachable at. All three fail instantly.
	candidates := []string{
		deadLoopbackAddr(t, "127.0.0.1"),
		unroutableAddr(t, "fdc5:7daa::1", 50051),
		unroutableAddr(t, "fdc5:7daa::2", 50051),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	target := newDialTargetCandidates("orin.local", candidates)
	conn, _, err := dialAgentLadderWithCerts(ctx, target, []config.CertificateInfo{selfSignedCLICert(t, 7)})
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("ladder returned no error though every candidate address was dead")
	}

	msg := err.Error()
	for _, cand := range candidates {
		host, _, splitErr := net.SplitHostPort(cand)
		if splitErr != nil {
			t.Fatalf("split %q: %v", cand, splitErr)
		}
		if !strings.Contains(msg, host) {
			t.Errorf("address %s was never named in the failure, so the ladder never tried it:\n  %s", host, msg)
		}
	}
	// The family tally is what tells a user whose device is IPv4-only that the
	// CLI did in fact look there.
	if !strings.Contains(msg, "1 IPv4 and 2 IPv6") {
		t.Errorf("error must tally the families actually tried:\n  %s", msg)
	}
}

// TestLadderStopsAtIdentityRefusalWithoutTryingMoreAddresses is the security
// regression guard for the candidate walk.
//
// "Try the next address" is the right response to silence. It is the WRONG
// response to a device that answered and identified itself as somebody else:
// walking on would keep dialing until some address produced a peer we accepted,
// which is a same-CA-host redirect with extra steps. The refusal must outrank
// the walk.
func TestLadderStopsAtIdentityRefusalWithoutTryingMoreAddresses(t *testing.T) {
	setTempConfig(t, &config.Config{}) // unpinned, so only the abort can refuse
	setPinCache(t)

	origMismatch := identityMismatchFn
	identityMismatchFn = func(*grpcclient.AgentConnection) (*certs.IdentityMismatchError, bool) {
		return &certs.IdentityMismatchError{WantOrg: 7, WantAsset: "42", GotOrg: 7, GotAsset: "43"}, true
	}
	plaintextCalls := 0
	origPlaintext := plaintextConnectFn
	plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		plaintextCalls++
		return grpcclient.NewFromConn(nil), nil
	}
	t.Cleanup(func() {
		identityMismatchFn = origMismatch
		plaintextConnectFn = origPlaintext
	})

	live := startPlaintextVersionAgent(t)
	// A live first address (so a handshake happens and the mismatch is read)
	// followed by two more the walk must never reach.
	candidates := []string{
		live,
		unroutableAddr(t, "fdc5:7daa::1", 50051),
		unroutableAddr(t, "fdc5:7daa::2", 50051),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, _, err := dialAgentLadderWithCerts(ctx,
		newDialTargetCandidates("orin.local", candidates),
		[]config.CertificateInfo{selfSignedCLICert(t, 7)})
	if conn != nil {
		conn.Close()
	}

	if err == nil {
		t.Fatal("a wrong-device abort produced no error")
	}
	if !errors.Is(err, errDeviceIdentityRefused) {
		t.Fatalf("err = %v, want an identity refusal", err)
	}
	if plaintextCalls != 0 {
		t.Errorf("plaintext rung attempted %d times after a wrong-device abort", plaintextCalls)
	}
	// The abort happened at the first address, so the later ones must be absent
	// from the diagnosis entirely.
	for _, cand := range candidates[1:] {
		host, _, _ := net.SplitHostPort(cand)
		if strings.Contains(err.Error(), host) {
			t.Errorf("the walk continued to %s after a wrong-device abort:\n  %s", host, err.Error())
		}
	}
	// A genuine mismatch keeps the unpin escape hatch — here the identity really
	// was compared and really did not match.
	if !strings.Contains(err.Error(), "device unpin") {
		t.Errorf("a real mismatch must still name the unpin escape hatch:\n  %s", err.Error())
	}
}

// TestResolveAddrCandidates_ReturnsEveryAddressIPv4First pins the resolution
// half: the woof.local shape from the original report, where the name answers to
// two ULA IPv6 addresses and one IPv4, and only the IPv4 is routable.
//
// Every address must come back — the old single-address form returned one and
// discarded the rest, which is what made one stale record fatal — ordered so the
// most likely to work is dialled first.
func TestResolveAddrCandidates_ReturnsEveryAddressIPv4First(t *testing.T) {
	orig := osLookupHostFn
	t.Cleanup(func() { osLookupHostFn = orig })
	osLookupHostFn = func(context.Context, string) ([]string, error) {
		return []string{"fdc5:7daa::1", "fdc5:7daa::2", "192.168.0.107"}, nil
	}

	got := resolveAddrCandidates(context.Background(), "woof.local:50051")
	want := []string{"192.168.0.107:50051", "[fdc5:7daa::1]:50051", "[fdc5:7daa::2]:50051"}
	if len(got) != len(want) {
		t.Fatalf("resolveAddrCandidates = %v, want all %d addresses %v", got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resolveAddrCandidates = %v, want %v", got, want)
		}
	}

	// resolveAddrOnce stays the first of the list, so every existing caller
	// keeps the IPv4-preferred single address it already relied on.
	if first := resolveAddrOnce(context.Background(), "woof.local:50051"); first != want[0] {
		t.Fatalf("resolveAddrOnce = %q, want %q", first, want[0])
	}
}

// The mDNS browse is the only way the shipped CLI resolves a ".local" name
// (built CGO_ENABLED=0, so the pure Go resolver never consults mDNS), and it had
// no address preference at all — it returned whichever record the browse
// surfaced first. It must now yield every match, ordered.
func TestResolveAddrCandidates_MDNSBrowseYieldsEveryMatchOrdered(t *testing.T) {
	origLookup, origBrowse := osLookupHostFn, lanBrowseFn
	t.Cleanup(func() { osLookupHostFn, lanBrowseFn = origLookup, origBrowse })

	osLookupHostFn = func(context.Context, string) ([]string, error) {
		return nil, errors.New("no such host")
	}
	lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) {
		return []models.LANDevice{
			{Hostname: "woof.local", IPAddress: "fdc5:7daa::1"},
			{Hostname: "other.local", IPAddress: "10.9.9.9"},
			{Hostname: "woof.local", IPAddress: "192.168.0.107"},
		}, nil
	}

	got := resolveAddrCandidates(context.Background(), "woof.local:50051")
	want := []string{"192.168.0.107:50051", "[fdc5:7daa::1]:50051"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("resolveAddrCandidates via mDNS = %v, want %v (IPv4 first, other.local excluded)", got, want)
	}
}

func TestOrderDialCandidates(t *testing.T) {
	t.Run("routable IPv6 outranks link-local and ULA", func(t *testing.T) {
		got := orderDialCandidates([]string{"fe80::1", "fdc5::1", "2606:4700::1"})
		want := []string{"2606:4700::1", "fe80::1", "fdc5::1"}
		if len(got) != len(want) {
			t.Fatalf("orderDialCandidates = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("orderDialCandidates = %v, want %v", got, want)
			}
		}
	})
	t.Run("IPv4 outranks every IPv6", func(t *testing.T) {
		got := orderDialCandidates([]string{"2606:4700::1", "192.168.0.107", "fdc5::1"})
		if len(got) != 3 || got[0] != "192.168.0.107" {
			t.Fatalf("orderDialCandidates = %v, want the IPv4 address first", got)
		}
	})
	t.Run("duplicates and unparseable entries are dropped", func(t *testing.T) {
		got := orderDialCandidates([]string{"10.0.0.1", "10.0.0.1", "", "not-an-ip"})
		if len(got) != 1 || got[0] != "10.0.0.1" {
			t.Fatalf("orderDialCandidates = %v, want exactly one usable address", got)
		}
	})
	t.Run("the walk is bounded", func(t *testing.T) {
		many := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"}
		if got := orderDialCandidates(many); len(got) != maxDialCandidates {
			t.Fatalf("orderDialCandidates returned %d addresses, want the walk capped at %d", len(got), maxDialCandidates)
		}
	})
	t.Run("a zoned link-local address survives", func(t *testing.T) {
		got := orderDialCandidates([]string{"fe80::1%en0"})
		if len(got) != 1 || got[0] != "fe80::1%en0" {
			t.Fatalf("orderDialCandidates = %v, want the zone preserved (USB link-local dials rely on it)", got)
		}
	})
}

func TestOrderRoutedDialCandidatesPrefersEthernetBeforeCandidateCap(t *testing.T) {
	original := dialCandidateRoutePreferenceFn
	dialCandidateRoutePreferenceFn = func(ip string) routePreference {
		if ip == "10.0.0.4" {
			return routeWired
		}
		return routeWireless
	}
	t.Cleanup(func() { dialCandidateRoutePreferenceFn = original })

	got := orderRoutedDialCandidates([]string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"})
	want := []string{"10.0.0.4", "10.0.0.1", "10.0.0.2"}
	if len(got) != len(want) {
		t.Fatalf("orderRoutedDialCandidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("orderRoutedDialCandidates = %v, want %v", got, want)
		}
	}
}

func TestCachedWiFiAddressDoesNotBypassFreshResolution(t *testing.T) {
	original := networkInterfaceRoutePreferenceFn
	originalRoute := routeInterfaceForIPFn
	t.Cleanup(func() {
		networkInterfaceRoutePreferenceFn = original
		routeInterfaceForIPFn = originalRoute
	})
	routeInterfaceForIPFn = func(string) string { return "en0" }

	networkInterfaceRoutePreferenceFn = func(string) routePreference { return routeWireless }
	if shouldUseCachedDeviceAddress("en0", "192.168.1.20") {
		t.Fatal("a cached Wi-Fi route must not suppress fresh multi-address resolution")
	}
	networkInterfaceRoutePreferenceFn = func(string) routePreference { return routeWired }
	if !shouldUseCachedDeviceAddress("en0", "192.168.1.30") {
		t.Fatal("a cached wired route should retain the last-known-good fast path")
	}
	networkInterfaceRoutePreferenceFn = func(string) routePreference { return routeUnknown }
	if !shouldUseCachedDeviceAddress("", "192.168.1.40") {
		t.Fatal("a legacy cache row with no interface metadata should retain the fast path")
	}

	networkInterfaceRoutePreferenceFn = func(name string) routePreference {
		if name == "en7" {
			return routeWired
		}
		return routeWireless
	}
	if shouldUseCachedDeviceAddress("en7", "192.168.1.50") {
		t.Fatal("a cached address routed over en0 must not be trusted merely because its sighting was labelled en7")
	}
}

// deadLoopbackAddr reserves and releases two consecutive ports on a loopback
// host, so both the address itself and its port+1 mTLS rung refuse instantly
// rather than hanging. It skips rather than fails when the host cannot be bound
// — an environment with no IPv6 loopback must not turn this into a red build.
func deadLoopbackAddr(t *testing.T, host string) string {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		first, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			t.Skipf("cannot listen on %s (%v); this environment cannot exercise the walk", host, err)
		}
		port := first.Addr().(*net.TCPAddr).Port
		second, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port+1)))
		first.Close()
		if err != nil {
			continue // port+1 taken; try another pair
		}
		second.Close()
		return net.JoinHostPort(host, strconv.Itoa(port))
	}
	t.Fatalf("could not find two consecutive free ports on %s", host)
	return ""
}

// unroutableAddr returns an address that fails to connect immediately, and skips
// the test if this host can somehow reach it.
//
// The candidate walk needs several DISTINCT dead hosts, and the obvious choice —
// extra 127/8 aliases — is wrong: macOS cannot bind them and, worse, connects to
// them time out after seconds instead of refusing, which would make the walk's
// cost the thing under test rather than its behaviour. An unrouted ULA IPv6
// address refuses instantly ("no route to host") on an ordinary LAN, and is also
// the exact shape of the report this fixes: a name answering to ULA IPv6
// addresses the device is not actually reachable at.
func unroutableAddr(t *testing.T, host string, port int) string {
	t.Helper()
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Skipf("%s is reachable here; this environment cannot exercise the walk", addr)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Skipf("%s takes %v to fail here rather than refusing outright; the walk would be timing-dependent", addr, elapsed)
	}
	return addr
}

// TestPrimaryRejectedOurCertIgnoresOtherCandidates is the truth table for the
// one decision the candidate walk must NOT influence.
//
// Whether the plaintext rung is offered is a question about target.Addr, the
// only address that rung dials. When the buckets were walk-wide instead, both
// directions broke: one unreachable extra candidate added a non-cert failure and
// un-suppressed the rung, handing out an unauthenticated connection where a
// single address would have refused; and one black-holed extra candidate (whose
// TLS handshake times out, which isCertRejectionError counts as a rejection)
// suppressed the rung for a primary that had legitimately earned it.
func TestPrimaryRejectedOurCertIgnoresOtherCandidates(t *testing.T) {
	cases := []struct {
		name string
		walk mtlsWalk
		want bool
	}{
		{"nothing failed", mtlsWalk{}, false},
		{"primary's own port rejected our cert", mtlsWalk{primaryOwnPortCertReject: true}, true},
		{"every primary mTLS-port failure was a cert rejection", mtlsWalk{primaryMTLSPortCertFails: 2}, true},
		{"primary was merely unreachable", mtlsWalk{primaryMTLSPortNonCertFails: 2}, false},
		{
			"a primary that was partly unreachable is not a rejection",
			mtlsWalk{primaryMTLSPortCertFails: 1, primaryMTLSPortNonCertFails: 1},
			false,
		},
		{
			"another candidate's cert rejection does not suppress the rung",
			mtlsWalk{anyCertRejection: true},
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.walk.primaryRejectedOurCert(); got != c.want {
				t.Fatalf("primaryRejectedOurCert() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestDialAddrScopesBucketsToThePrimary guards the mechanism behind the truth
// table above: identical failures at the same address must move the suppression
// buckets when that address is the primary and must not when it is not.
//
// Without this, nothing catches the buckets silently going walk-wide again — the
// end-to-end tests cannot, because reproducing the harmful direction needs an
// address whose TLS handshake TIMES OUT (which isCertRejectionError counts as a
// rejection) rather than one that refuses instantly, and a test that waits out a
// handshake budget to prove it is a test that gets deleted for being slow.
func TestDialAddrScopesBucketsToThePrimary(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)

	dead := deadLoopbackAddr(t, "127.0.0.1")
	newWalk := func() *mtlsWalk {
		return &mtlsWalk{
			target:     newDialTarget("orin.local", dead),
			allCerts:   []config.CertificateInfo{selfSignedCLICert(t, 7)},
			probeOrder: []int{0},
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	asPrimary := newWalk()
	if conn, refusal := asPrimary.dialAddr(ctx, dead, true); conn != nil || refusal != nil {
		t.Fatalf("dialAddr(primary) = (%v, %v), want nothing answered", conn, refusal)
	}
	notPrimary := newWalk()
	if conn, refusal := notPrimary.dialAddr(ctx, dead, false); conn != nil || refusal != nil {
		t.Fatalf("dialAddr(non-primary) = (%v, %v), want nothing answered", conn, refusal)
	}

	// The primary run accounts for its failures and records its own last error.
	if asPrimary.primaryMTLSPortCertFails+asPrimary.primaryMTLSPortNonCertFails == 0 {
		t.Error("the primary's failures were not accounted for in its own buckets")
	}
	if asPrimary.primaryLastErr == nil {
		t.Error("primaryLastErr = nil, want the primary's failure kept for chooseRejectionError")
	}
	// The non-primary run must touch none of it, while still logging the attempt
	// so the unreachable message can name the address.
	if notPrimary.primaryOwnPortCertReject ||
		notPrimary.primaryMTLSPortCertFails != 0 ||
		notPrimary.primaryMTLSPortNonCertFails != 0 ||
		notPrimary.primaryLastErr != nil {
		t.Errorf("a non-primary candidate voted on the primary's verdict: %+v", notPrimary)
	}
	if len(notPrimary.attempts) == 0 {
		t.Error("a non-primary candidate logged no attempt, so the failure could not name it")
	}
	if notPrimary.lastMTLSErr == nil {
		t.Error("lastMTLSErr = nil, want every candidate's failure kept for diagnostics")
	}
}

// An extra unreachable candidate must not change what happens at the primary.
//
// The primary here is a live plaintext-only agent, which legitimately earns the
// plaintext rung: its mTLS probes fail as plain unreachability, not as cert
// rejections. Appending a black-holed address must leave that untouched. When
// the suppression buckets were walk-wide, the black-holed candidate's own-port
// TLS handshake timed out, isCertRejectionError counted that as a rejection, and
// the rung was suppressed — breaking a connect that worked before.
func TestExtraDeadCandidateDoesNotSuppressThePlaintextRung(t *testing.T) {
	setTempConfig(t, &config.Config{}) // unpinned: the rung is allowed
	setPinCache(t)

	live := startPlaintextVersionAgent(t)

	plaintextAddrs := []string{}
	origPlaintext := plaintextConnectFn
	plaintextConnectFn = func(_ context.Context, address string) (*grpcclient.AgentConnection, error) {
		plaintextAddrs = append(plaintextAddrs, address)
		return grpcclient.NewFromConn(nil), nil
	}
	t.Cleanup(func() { plaintextConnectFn = origPlaintext })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates := []string{live, unroutableAddr(t, "fdc5:7daa::1", 50051)}
	conn, _, err := dialAgentLadderWithCerts(ctx,
		newDialTargetCandidates("orin.local", candidates),
		[]config.CertificateInfo{selfSignedCLICert(t, 7)})
	if conn != nil {
		conn.Close()
	}
	if err != nil {
		t.Fatalf("err = %v, want the plaintext rung to be reached for an unpinned host", err)
	}
	if len(plaintextAddrs) != 1 {
		t.Fatalf("plaintext rung dialled %v, want exactly one attempt", plaintextAddrs)
	}
	// And it must dial the PRIMARY, not whichever candidate happened to be last.
	if plaintextAddrs[0] != live {
		t.Errorf("plaintext rung dialled %q, want the primary address %q", plaintextAddrs[0], live)
	}
}

// The tally folds by address so two ports on one address count once, but a
// link-local address is identified by its zone as much as by its digits: two
// interfaces carrying fe80::1 are two places to dial, and describeDialAttempts
// lists them as two. Folding the zone away made the summary say "1 IPv6" under a
// list of two — the reader is then told the count and shown a contradiction.
func TestAddrFamilyCountsKeepsZonesApartAndFoldsPorts(t *testing.T) {
	cases := []struct {
		name   string
		addrs  []string
		v4, v6 int
	}{
		{
			name:  "same link-local on two interfaces counts twice",
			addrs: []string{"[fe80::1%en0]:50051", "[fe80::1%en1]:50051"},
			v6:    2,
		},
		{
			name:  "one zoned address on two ports counts once",
			addrs: []string{"[fe80::1%en0]:50051", "[fe80::1%en0]:50052"},
			v6:    1,
		},
		{
			name:  "mixed families, ports folded",
			addrs: []string{"10.0.0.5:50051", "10.0.0.5:50052", "[fdc5::1]:50051", "[fe80::1%en0]:50051"},
			v4:    1,
			v6:    2,
		},
		{
			name:  "unresolved names are not counted as either family",
			addrs: []string{"woof.local:50051"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v4, v6 := addrFamilyCounts(c.addrs)
			if v4 != c.v4 || v6 != c.v6 {
				t.Errorf("addrFamilyCounts(%v) = (%d, %d), want (%d, %d)", c.addrs, v4, v6, c.v4, c.v6)
			}
		})
	}
}

func TestDescribeAddrFamilies(t *testing.T) {
	cases := []struct {
		v4, v6 int
		want   string
	}{
		{1, 0, "1 IPv4 address"},
		{0, 2, "2 IPv6 addresses"},
		{1, 2, "1 IPv4 and 2 IPv6 addresses"},
		{0, 0, "no resolved addresses"},
	}
	for _, c := range cases {
		if got := describeAddrFamilies(c.v4, c.v6); got != c.want {
			t.Errorf("describeAddrFamilies(%d, %d) = %q, want %q", c.v4, c.v6, got, c.want)
		}
	}
}

// The attempt log carries one entry per address AND port; the message must fold
// them by address, or three addresses read as six places we looked and the tally
// contradicts the list.
func TestDescribeDialAttemptsFoldsPortsByAddress(t *testing.T) {
	attempts := []mtlsAttemptError{
		{addr: "10.0.0.5:50051", err: errors.New("connection refused")},
		{addr: "10.0.0.5:50052", err: errors.New("connection refused")},
		{addr: "[fdc5::1]:50051", err: errors.New("no route to host")},
	}
	got := describeDialAttempts(attempts)
	if len(got) != 2 {
		t.Fatalf("describeDialAttempts = %v, want one entry per address (2), not per rung", got)
	}
	if !strings.HasPrefix(got[0], "10.0.0.5 (") || !strings.HasPrefix(got[1], "fdc5::1 (") {
		t.Fatalf("describeDialAttempts = %v, want each address with its reason", got)
	}
	if !strings.Contains(got[1], "no route to host") {
		t.Fatalf("describeDialAttempts = %v, want the last reason for each address", got)
	}
}

func TestCondenseDialReasonSurfacesTheTransportCause(t *testing.T) {
	// The real shape: the two words that matter are at the very end, so a plain
	// length cap would keep only boilerplate.
	grpcErr := `rpc error: code = Unavailable desc = connection error: desc = "transport: Error while dialing: dial tcp 10.0.0.5:50051: connect: connection refused"`
	if got := condenseDialReason(grpcErr); got != "connection refused" {
		t.Errorf("condenseDialReason = %q, want the transport cause", got)
	}
	if got := condenseDialReason(`... dial tcp [fdc5::1]:50051: connect: no route to host"`); got != "no route to host" {
		t.Errorf("condenseDialReason = %q, want %q", got, "no route to host")
	}
	if got := condenseDialReason("connection\n  refused"); got != "connection refused" {
		t.Errorf("condenseDialReason = %q, want the whitespace flattened", got)
	}
	// Anything unrecognised still gets flattened and bounded rather than dumped.
	long := condenseDialReason(strings.Repeat("zq ", 500))
	if len([]rune(long)) > 95 {
		t.Errorf("condenseDialReason returned %d chars, want a bounded clause", len([]rune(long)))
	}
}

// A dialTarget built the old way — one address, no Candidates — must behave
// exactly as it did before the walk existed. Every existing caller does this.
func TestDialCandidatesDefaultsToTheSingleAddress(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)

	single := newDialTarget("orin.local", "10.0.0.9:50051")
	if got := single.dialCandidates(); len(got) != 1 || got[0] != "10.0.0.9:50051" {
		t.Fatalf("dialCandidates = %v, want just the single address", got)
	}
	if single.Candidates != nil {
		t.Errorf("Candidates = %v, want nil for a single-address target", single.Candidates)
	}

	multi := newDialTargetCandidates("orin.local", []string{"10.0.0.9:50051", "10.0.0.8:50051"})
	if multi.Addr != "10.0.0.9:50051" {
		t.Errorf("Addr = %q, want the first candidate to stay the primary address", multi.Addr)
	}
	if got := multi.dialCandidates(); len(got) != 2 {
		t.Errorf("dialCandidates = %v, want both addresses", got)
	}

	if got := (dialTarget{}).dialCandidates(); got != nil {
		t.Errorf("dialCandidates on an empty target = %v, want nil", got)
	}
}

// connectWithAutoTLSDiagnostics must hand the ladder every address a cache-miss
// resolution produced, not just the first — otherwise the walk is unreachable
// from the path almost every command takes.
func TestConnectWithAutoTLSDiagnostics_PassesAllResolvedCandidatesToLadder(t *testing.T) {
	setTempConfig(t, &config.Config{})

	origLoad, origLookup, origLadder := deviceCacheLoadFn, osLookupHostFn, dialAgentLadderFn
	t.Cleanup(func() {
		deviceCacheLoadFn, osLookupHostFn, dialAgentLadderFn = origLoad, origLookup, origLadder
	})

	// Empty cache, so this is the cache-miss resolution path.
	cachePath := filepath.Join(t.TempDir(), "devices.json")
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(cachePath) }

	osLookupHostFn = func(context.Context, string) ([]string, error) {
		return []string{"fdc5:7daa::1", "192.168.0.107"}, nil
	}

	var seen []string
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		seen = target.dialCandidates()
		return nil, nil, errors.New("connection refused")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := connectWithAutoTLSDiagnostics(ctx, "woof.local:50051"); err == nil {
		t.Fatal("expected the dial failure to surface")
	}

	want := []string{"192.168.0.107:50051", "[fdc5:7daa::1]:50051"}
	if len(seen) != len(want) {
		t.Fatalf("ladder received %v, want every resolved address %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("ladder received %v, want %v", seen, want)
		}
	}
}
