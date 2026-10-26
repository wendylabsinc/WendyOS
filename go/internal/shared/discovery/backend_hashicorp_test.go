//go:build linux || windows

package discovery

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/mdns"
)

// ── hashicorpEntryToService (pure logic, no network) ──────────────────────

func TestHashicorpEntryToServiceFiltersWrongServiceType(t *testing.T) {
	entry := &mdns.ServiceEntry{
		Name: "phone._remotepairing._tcp.local.",
		Host: "phone.local.",
		Port: 1234,
	}

	if _, ok := hashicorpEntryToService(entry, nil, wendyServiceType); ok {
		t.Fatal("hashicorpEntryToService returned true for a non-matching service type")
	}
}

func TestHashicorpEntryToServicePrefersIPv4AndParsesTXT(t *testing.T) {
	iface := &net.Interface{Name: "eth0"}
	entry := &mdns.ServiceEntry{
		Name:       "wendyos-prudent-lark._wendyos._udp.local.",
		Host:       "wendyos-prudent-lark.local.",
		AddrV4:     net.ParseIP("192.168.1.42"),
		AddrV6:     net.ParseIP("fe80::1"),
		Port:       50051,
		InfoFields: []string{"id=agent-id", "displayname=Prudent Lark", "tls=true"},
	}

	svc, ok := hashicorpEntryToService(entry, iface, wendyServiceType)
	if !ok {
		t.Fatal("hashicorpEntryToService returned false for a matching entry")
	}
	if svc.InstanceName != "wendyos-prudent-lark" {
		t.Errorf("InstanceName = %q, want %q", svc.InstanceName, "wendyos-prudent-lark")
	}
	if svc.Hostname != "wendyos-prudent-lark.local" {
		t.Errorf("Hostname = %q, want %q", svc.Hostname, "wendyos-prudent-lark.local")
	}
	if svc.IPAddress != "192.168.1.42" {
		t.Errorf("IPAddress = %q, want the IPv4 address preferred over IPv6", svc.IPAddress)
	}
	if svc.Port != 50051 {
		t.Errorf("Port = %d, want 50051", svc.Port)
	}
	if svc.TXTRecords["displayname"] != "Prudent Lark" || svc.TXTRecords["tls"] != "true" {
		t.Errorf("TXTRecords = %v, want displayname/tls parsed", svc.TXTRecords)
	}
	if svc.InterfaceName != "eth0" {
		t.Errorf("InterfaceName = %q, want %q", svc.InterfaceName, "eth0")
	}
}

func TestHashicorpEntryToServiceIPv6LinkLocalGetsZoneSuffix(t *testing.T) {
	iface := &net.Interface{Name: "eth0"}
	entry := &mdns.ServiceEntry{
		Name:   "wendyos-foo._wendyos._udp.local.",
		Host:   "wendyos-foo.local.",
		AddrV6: net.ParseIP("fe80::1"),
		Port:   50051,
	}

	svc, ok := hashicorpEntryToService(entry, iface, wendyServiceType)
	if !ok {
		t.Fatal("hashicorpEntryToService returned false for a matching entry")
	}
	if svc.IPAddress != "fe80::1%eth0" {
		t.Errorf("IPAddress = %q, want zone-scoped fe80::1%%eth0", svc.IPAddress)
	}
}

func TestHashicorpEntryToServiceNilInterfaceOmitsZoneAndName(t *testing.T) {
	entry := &mdns.ServiceEntry{
		Name:   "wendyos-foo._wendyos._udp.local.",
		Host:   "wendyos-foo.local.",
		AddrV6: net.ParseIP("fe80::1"),
		Port:   50051,
	}

	// nil is the sweep target Windows uses for its default/all-interface
	// query, where hashicorp/mdns cannot say which adapter answered.
	svc, ok := hashicorpEntryToService(entry, nil, wendyServiceType)
	if !ok {
		t.Fatal("hashicorpEntryToService returned false for a matching entry")
	}
	if svc.InterfaceName != "" {
		t.Errorf("InterfaceName = %q, want empty for a nil interface", svc.InterfaceName)
	}
	if svc.IPAddress != "fe80::1" {
		t.Errorf("IPAddress = %q, want unscoped (no interface to scope it to)", svc.IPAddress)
	}
}

// ── parseMDNSInfoFields ─────────────────────────────────────────────

func TestParseMDNSInfoFields(t *testing.T) {
	tests := []struct {
		name   string
		fields []string
		want   map[string]string
	}{
		{
			name:   "empty",
			fields: nil,
			want:   map[string]string{},
		},
		{
			name:   "no tls record",
			fields: []string{"id=some-device", "name=my-device"},
			want:   map[string]string{"id": "some-device", "name": "my-device"},
		},
		{
			name:   "tls=true for provisioned device",
			fields: []string{"wendyosdevice=prov-uuid", "tls=true"},
			want:   map[string]string{"wendyosdevice": "prov-uuid", "tls": "true"},
		},
		{
			name:   "tls=false is not treated as mTLS",
			fields: []string{"wendyosdevice=some-uuid", "tls=false"},
			want:   map[string]string{"wendyosdevice": "some-uuid", "tls": "false"},
		},
		{
			name:   "entry without equals sign is skipped",
			fields: []string{"noequals", "key=val"},
			want:   map[string]string{"key": "val"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseMDNSInfoFields(tt.fields)
			if len(got) != len(tt.want) {
				t.Fatalf("parseMDNSInfoFields(%v) returned %d entries, want %d: got %v", tt.fields, len(got), len(tt.want), got)
			}
			for k, want := range tt.want {
				if got[k] != want {
					t.Fatalf("parseMDNSInfoFields()[%q] = %q, want %q", k, got[k], want)
				}
			}
			// Verify tls→IsMTLS mapping works correctly.
			isMTLS := got["tls"] == "true"
			wantMTLS := tt.want["tls"] == "true"
			if isMTLS != wantMTLS {
				t.Fatalf("tls→IsMTLS = %v, want %v", isMTLS, wantMTLS)
			}
		})
	}
}

// ── hashicorpSweepTargets (pure logic, injected interface list) ───────────

func TestHashicorpSweepTargetsIncludesNilOnWindowsOnly(t *testing.T) {
	origIfaces := ifaceListFn
	t.Cleanup(func() { ifaceListFn = origIfaces })

	fake := []net.Interface{{Name: "eth-fake-0"}}
	ifaceListFn = func() []net.Interface { return fake }

	targets := hashicorpSweepTargets()

	wantLen := len(fake)
	wantNil := runtime.GOOS == "windows"
	if wantNil {
		wantLen++
	}
	if len(targets) != wantLen {
		t.Fatalf("hashicorpSweepTargets() returned %d targets, want %d", len(targets), wantLen)
	}

	hasNil := false
	for _, tgt := range targets {
		if tgt == nil {
			hasNil = true
		}
	}
	if hasNil != wantNil {
		t.Errorf("nil (default-interface) target present = %v, want %v (runtime.GOOS=%s)", hasNil, wantNil, runtime.GOOS)
	}
}

// ── parallel sweep timing (pure logic, injected interfaces + query fn) ────

// TestHashicorpSweepOnceQueriesInterfacesInParallel is the property the
// streaming engine depends on for "instant" discovery: a sweep across
// several interfaces must not serialize them, or the effective latency for a
// device on the Nth interface would scale with N. sweepQueryFn is faked to
// simulate a slow query without touching the network, so the test is
// deterministic and requires no multicast privileges.
func TestHashicorpSweepOnceQueriesInterfacesInParallel(t *testing.T) {
	origIfaces := ifaceListFn
	origQuery := sweepQueryFn
	t.Cleanup(func() {
		ifaceListFn = origIfaces
		sweepQueryFn = origQuery
	})

	fakeIfaces := []net.Interface{{Name: "eth-fake-0"}, {Name: "eth-fake-1"}, {Name: "eth-fake-2"}}
	ifaceListFn = func() []net.Interface { return fakeIfaces }

	const perQueryDelay = 100 * time.Millisecond

	var mu sync.Mutex
	var calls []string
	sweepQueryFn = func(ctx context.Context, iface *net.Interface, _ string, _ chan *mdns.ServiceEntry, _ time.Duration) error {
		name := "nil"
		if iface != nil {
			name = iface.Name
		}
		mu.Lock()
		calls = append(calls, name)
		mu.Unlock()

		select {
		case <-time.After(perQueryDelay):
		case <-ctx.Done():
		}
		return nil
	}

	start := time.Now()
	hashicorpSweepOnce(context.Background(), wendyServiceType, func(MDNSService) {})
	elapsed := time.Since(start)

	if elapsed >= 2*perQueryDelay {
		t.Errorf("sweep across %d fake interfaces took %v, want < %v (interfaces must be queried in parallel, not serially)",
			len(fakeIfaces), elapsed, 2*perQueryDelay)
	}

	mu.Lock()
	defer mu.Unlock()
	wantCalls := len(fakeIfaces)
	if runtime.GOOS == "windows" {
		wantCalls++ // the nil default-interface target
	}
	if len(calls) != wantCalls {
		t.Errorf("sweepQueryFn called %d times, want %d (calls: %v)", len(calls), wantCalls, calls)
	}
}

// TestHashicorpQueryInterfaceReadsEntriesOnlyAfterTheQueryReturns pins the
// rule that keeps this backend free of data races, and is the regression test
// for the one the live-advertise test hit on CI.
//
// hashicorp/mdns hands a *ServiceEntry over params.Entries as soon as the
// entry looks complete, but keeps that same struct in its own in-progress map
// and goes on writing to it — Host, Port, InfoFields, AddrV4, AddrV6 — for
// every later packet naming it, which on a host running several interface
// queries at once (each one's socket seeing the others' replies too) is the
// normal case rather than the exotic one. The channel hand-off orders only the
// writes made before the send, its send is non-blocking so it never waits for
// the receiver, and there is no lock to share: touching any field of a
// delivered entry before the query has returned is therefore a race no
// consumer can synchronize away, and the only fix available here is not to
// touch it until then.
//
// The fake below reproduces exactly that shape, so this test fails under
// -race against any implementation that converts entries while the query is
// still running — including the "forward each entry the moment it arrives"
// one this replaced.
func TestHashicorpQueryInterfaceReadsEntriesOnlyAfterTheQueryReturns(t *testing.T) {
	origQuery := sweepQueryFn
	t.Cleanup(func() { sweepQueryFn = origQuery })

	sweepQueryFn = func(_ context.Context, _ *net.Interface, _ string, entriesCh chan *mdns.ServiceEntry, _ time.Duration) error {
		entry := &mdns.ServiceEntry{
			Name:       "wendyos-racer._wendyos._udp.local.",
			Host:       "first.local.",
			Port:       1111,
			AddrV4:     net.ParseIP("10.0.0.1"),
			InfoFields: []string{"round=first"},
		}
		entriesCh <- entry

		// The later packets. Repeated to widen the window a concurrent reader
		// would have to land in, so the failure is reliable rather than
		// occasional.
		for i := 0; i < 500; i++ {
			entry.Host = "second.local."
			entry.Port = 2222
			entry.AddrV4 = net.ParseIP("10.0.0.2")
			entry.InfoFields = []string{"round=second"}
		}
		return nil
	}

	var got []MDNSService
	hashicorpQueryInterface(context.Background(), &net.Interface{Name: "eth-fake-0"}, wendyServiceType,
		func(svc MDNSService) { got = append(got, svc) })

	if len(got) != 1 {
		t.Fatalf("emitted %d services, want 1: %+v", len(got), got)
	}
	// Reading after the query has returned also means reading a settled entry
	// rather than a half-updated one, so the sweep reports what the query
	// finished on.
	if got[0].Hostname != "second.local" || got[0].Port != 2222 ||
		got[0].IPAddress != "10.0.0.2" || got[0].TXTRecords["round"] != "second" {
		t.Errorf("emitted %+v, want the entry as the query left it (second.local/2222/10.0.0.2/round=second)", got[0])
	}
}

// ── live fixture (advertises via hashicorp/mdns's own server) ─────────────

// TestHashicorpStreamBackendLiveAdvertise is the end-to-end property this
// backend exists for: an advertised service arrives within one sweep, and
// arrives again after a genuine re-query — proving the backend keeps
// re-sweeping rather than querying once and going quiet. It advertises its
// own hashicorp/mdns server in-process (mirroring hashicorp/mdns's own
// TestServer_Lookup) rather than depending on a real device on the network,
// and skips outright when this host cannot exercise it: no multicast
// listener available, or no eligible network interface for
// hashicorpSweepTargets to query in the first place.
//
// hashicorp/mdns's own client.query never returns before params.Timeout
// elapses (it keeps listening for further answers on every interface for the
// full window), even once it has already delivered a matching entry. That
// gives this test a provable lower bound — not just a probabilistic one — on
// the gap between a sweep's replies and the next sweep's: no reply from sweep
// N+1 can be emitted before sweep N's queries all return, which cannot happen
// before hashicorpSweepTimeout has elapsed since sweep N started. So instead
// of treating "the second value read off the arrivals channel" as proof a
// re-query happened (multiple eligible interfaces on this host can each
// deliver their own answer for the SAME sweep, arriving back to back), this
// drains every arrival that shows up within a quiet window short enough that
// it cannot possibly reach into the next sweep, and only then waits for the
// next arrival — which is thereby guaranteed to belong to a later sweep.
func TestHashicorpStreamBackendLiveAdvertise(t *testing.T) {
	if len(hashicorpSweepTargets()) == 0 {
		t.Skip("no eligible mDNS interfaces on this host; hashicorpStreamBackend would never query")
	}

	const serviceType = "_wendy-hcbackendtest._tcp"
	instance := fmt.Sprintf("wendy-hcbackendtest-%d", os.Getpid())
	const port = 51260
	want := map[string]string{"displayname": "HC Backend Test", "tls": "true"}

	// Advertise our own address instead of requiring the test host's name to
	// resolve in DNS. Actual multicast queries and repeated sweeps still run.
	host := instance + ".local."
	svc, err := mdns.NewMDNSService(instance, serviceType, "", host, port, []net.IP{net.IPv4(127, 0, 0, 1)}, txtSliceFrom(want))
	if err != nil {
		t.Fatalf("mdns.NewMDNSService: %v", err)
	}
	server, err := mdns.NewServer(&mdns.Config{Zone: svc})
	if err != nil {
		t.Skipf("cannot start an mDNS server (no multicast listener available in this environment): %v", err)
	}
	t.Cleanup(func() { _ = server.Shutdown() })

	origRequery, origTimeout := hashicorpRequeryDelay, hashicorpSweepTimeout
	hashicorpSweepTimeout = 500 * time.Millisecond
	hashicorpRequeryDelay = 300 * time.Millisecond
	t.Cleanup(func() {
		hashicorpRequeryDelay = origRequery
		hashicorpSweepTimeout = origTimeout
	})

	// Short enough that a second reply from the SAME sweep (another eligible
	// interface answering) cannot plausibly be spaced out this far on a real
	// LAN, but long enough that it is well clear of ordinary multicast RTT
	// jitter. Deliberately well under hashicorpSweepTimeout so draining it out
	// can never itself run into the next sweep's replies.
	const quietWindow = 200 * time.Millisecond
	const waitFor = 5 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	type arrival struct {
		svc MDNSService
		at  time.Time
	}
	arrivals := make(chan arrival, 64)
	done := make(chan error, 1)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		done <- hashicorpStreamBackend(ctx, serviceType, func(got MDNSService) {
			if got.InstanceName != instance {
				return
			}
			select {
			case arrivals <- arrival{svc: got, at: time.Now()}:
			case <-ctx.Done():
			}
		})
	}()
	// Fatal assertion paths must also join before cleanup restores the query
	// timing globals used by the worker.
	t.Cleanup(func() {
		cancel()
		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
			t.Error("mDNS fixture worker did not stop during cleanup")
		}
	})

	var first arrival
	select {
	case first = <-arrivals:
	case <-time.After(waitFor):
		t.Fatal("advertised service did not arrive within the first sweep")
	}

	// Drain any further same-sweep replies (other eligible interfaces
	// answering); only a genuine quiet period proves sweep 1 is done emitting.
	sweep1Last := first.at
drain:
	for {
		select {
		case a := <-arrivals:
			sweep1Last = a.at
		case <-time.After(quietWindow):
			break drain
		}
	}

	var second arrival
	select {
	case second = <-arrivals:
	case <-time.After(waitFor):
		t.Fatal("advertised service did not arrive again after a re-query (second sweep never happened)")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("hashicorpStreamBackend returned %v, want nil after ctx cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hashicorpStreamBackend did not return after ctx cancellation (a goroutine outlived the call)")
	}

	if gap := second.at.Sub(sweep1Last); gap < hashicorpSweepTimeout {
		t.Errorf("gap between sweep-1's last reply and the next arrival = %v, want >= hashicorpSweepTimeout (%v) — "+
			"this looks like two replies from the SAME sweep rather than a genuine re-query", gap, hashicorpSweepTimeout)
	}
	for _, a := range []arrival{first, second} {
		if a.svc.Port != port {
			t.Errorf("Port = %d, want %d", a.svc.Port, port)
		}
		for k, v := range want {
			if a.svc.TXTRecords[k] != v {
				t.Errorf("TXT[%q] = %q, want %q", k, a.svc.TXTRecords[k], v)
			}
		}
	}
}

// ── hashicorpStreamBackend requery cadence + ctx-done contract (no network) ─

// TestHashicorpStreamBackendRequeriesOnCadenceAndJoinsOnCancel exercises
// hashicorpStreamBackend itself (not just its sweepOnce/sweepTargets pieces)
// with the ifaceListFn and sweepQueryFn seams faked out, so it needs no
// network and never skips — unlike TestHashicorpStreamBackendLiveAdvertise,
// this always runs. It pins two properties: the backend keeps re-sweeping on
// the hashicorpRequeryDelay cadence rather than querying once and stopping,
// and once ctx is cancelled it returns nil promptly with every goroutine
// already joined — checked by confirming sweepQueryFn is never called again
// after hashicorpStreamBackend has returned.
func TestHashicorpStreamBackendRequeriesOnCadenceAndJoinsOnCancel(t *testing.T) {
	origIfaces := ifaceListFn
	origQuery := sweepQueryFn
	origRequery, origTimeout := hashicorpRequeryDelay, hashicorpSweepTimeout
	t.Cleanup(func() {
		ifaceListFn = origIfaces
		sweepQueryFn = origQuery
		hashicorpRequeryDelay = origRequery
		hashicorpSweepTimeout = origTimeout
	})

	ifaceListFn = func() []net.Interface { return []net.Interface{{Name: "eth-fake-0"}} }
	hashicorpRequeryDelay = 10 * time.Millisecond
	hashicorpSweepTimeout = 200 * time.Millisecond

	var mu sync.Mutex
	var callCount int
	sweepQueryFn = func(ctx context.Context, _ *net.Interface, _ string, _ chan *mdns.ServiceEntry, _ time.Duration) error {
		mu.Lock()
		callCount++
		mu.Unlock()
		return nil // fake query: no entries, returns immediately
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- hashicorpStreamBackend(ctx, wendyServiceType, func(MDNSService) {})
	}()

	// (a) Requery cadence: poll (bounded, generous) rather than a fixed sleep
	// so this is fast on a quick machine and not flaky on a slow one.
	const wantSweeps = 3
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := callCount
		mu.Unlock()
		if n >= wantSweeps {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d sweeps ran within 3s at a %v requery delay, want >= %d (backend is not re-querying on cadence)",
				n, hashicorpRequeryDelay, wantSweeps)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// (b) ctx-done contract: cancel, and require a prompt, fully-joined return.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("hashicorpStreamBackend returned %v, want nil after ctx cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hashicorpStreamBackend did not return promptly after ctx cancellation")
	}

	// No goroutine may outlive the call: once done has fired, sweepQueryFn
	// must never be invoked again. Settle for comfortably longer than the
	// (10ms) requery cadence so a lingering sweeper would certainly show up.
	mu.Lock()
	countAtReturn := callCount
	mu.Unlock()

	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	countAfterSettle := callCount
	mu.Unlock()
	if countAfterSettle != countAtReturn {
		t.Errorf("sweepQueryFn was called %d more time(s) after hashicorpStreamBackend returned — a goroutine outlived the call",
			countAfterSettle-countAtReturn)
	}
}

// txtSliceFrom encodes a TXT record map into hashicorp/mdns's
// []string{"key=value", ...} form for MDNSService.
func txtSliceFrom(records map[string]string) []string {
	out := make([]string, 0, len(records))
	for k, v := range records {
		out = append(out, fmt.Sprintf("%s=%s", k, v))
	}
	return out
}
