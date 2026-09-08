package discovery

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// txtWire encodes entries into DNS-SD TXT wire format.
func txtWire(entries ...string) []byte {
	var out []byte
	for _, e := range entries {
		out = append(out, byte(len(e)))
		out = append(out, e...)
	}
	return out
}

func TestParseTXTRecord(t *testing.T) {
	cases := []struct {
		name string
		txt  []byte
		want map[string]string
	}{
		{name: "empty", txt: nil, want: map[string]string{}},
		{
			name: "key=value pairs",
			txt:  txtWire("tls=true", "assetid=338"),
			want: map[string]string{"tls": "true", "assetid": "338"},
		},
		{
			// The reason this parser exists: dns-sd's display format escaped
			// spaces as "\ " and the old code had to undo that by hand.
			name: "value containing spaces is preserved verbatim",
			txt:  txtWire("displayname=Tom Rpi4"),
			want: map[string]string{"displayname": "Tom Rpi4"},
		},
		{
			name: "value containing an equals sign keeps it",
			txt:  txtWire("token=abc=def"),
			want: map[string]string{"token": "abc=def"},
		},
		{
			name: "attribute with no value maps to empty string",
			txt:  txtWire("standalone"),
			want: map[string]string{"standalone": ""},
		},
		{
			name: "first occurrence of a repeated key wins (RFC 6763 6.4)",
			txt:  txtWire("dup=first", "dup=second"),
			want: map[string]string{"dup": "first"},
		},
		{
			name: "entry with an empty key is skipped",
			txt:  txtWire("=orphaned", "tls=true"),
			want: map[string]string{"tls": "true"},
		},
		{
			// A zero-length string must not mask the entries after it.
			name: "zero length string is skipped, later entries still decoded",
			txt:  append(append(txtWire("displayname=Tom Rpi4"), 0x00), txtWire("tls=true")...),
			want: map[string]string{"displayname": "Tom Rpi4", "tls": "true"},
		},
		{
			name: "length overrunning the buffer keeps what was decoded",
			txt:  append(txtWire("tls=true"), 0x40, 'a', 'b'),
			want: map[string]string{"tls": "true"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseTXTRecord(tc.txt); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseTXTRecord(%v) = %v, want %v", tc.txt, got, tc.want)
			}
		})
	}
}

// TestLANDeviceFromServiceIdentityFallback pins the identity floor of the
// mDNS→device mapper: a sighting whose hostname (and displayname TXT record)
// is missing still yields a named, keyable device by falling back to the
// DNS-SD instance name. Without this an unresolved answer produces
// ID/DisplayName/Hostname all empty, which every cache and dedup key
// (discoverycache.Key("","") == "") collapses into a single nameless,
// un-dialable row.
func TestLANDeviceFromServiceIdentityFallback(t *testing.T) {
	cases := []struct {
		name            string
		svc             MDNSService
		wantID, wantDsp string
	}{
		{
			name:    "no hostname falls back to the instance name",
			svc:     MDNSService{InstanceName: "orin-nano", Port: defaultAgentPort},
			wantID:  "orin-nano",
			wantDsp: "orin-nano",
		},
		{
			name:    "empty displayname TXT record falls back too",
			svc:     MDNSService{InstanceName: "orin-nano", TXTRecords: map[string]string{"displayname": ""}},
			wantID:  "orin-nano",
			wantDsp: "orin-nano",
		},
		{
			name:    "TXT device id still wins over the instance name",
			svc:     MDNSService{InstanceName: "orin-nano", TXTRecords: map[string]string{"wendyosdevice": "uuid-1"}},
			wantID:  "uuid-1",
			wantDsp: "orin-nano",
		},
		{
			name:    "a resolved hostname still wins over the instance name",
			svc:     MDNSService{InstanceName: "instance", Hostname: "orin.local"},
			wantID:  "orin",
			wantDsp: "orin",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			dev := lanDeviceFromService(tt.svc)
			if dev.ID != tt.wantID || dev.DisplayName != tt.wantDsp {
				t.Fatalf("ID/DisplayName = %q/%q; want %q/%q", dev.ID, dev.DisplayName, tt.wantID, tt.wantDsp)
			}
		})
	}
}

func TestPreferIPv4Addr(t *testing.T) {
	cases := []struct {
		name  string
		addrs []string
		want  string
	}{
		{name: "empty", addrs: nil, want: ""},
		{
			name:  "IPv4 preferred over an earlier IPv6",
			addrs: []string{"2600:1011:a003:4221:be41:6859:13c0:f7", "192.168.0.159"},
			want:  "192.168.0.159",
		},
		{
			name:  "first IPv4 wins",
			addrs: []string{"192.168.0.159", "10.0.0.5"},
			want:  "192.168.0.159",
		},
		{
			name:  "falls back to first address when no IPv4",
			addrs: []string{"2001:db8::1", "2001:db8::2"},
			want:  "2001:db8::1",
		},
		{
			name:  "unparseable entries are skipped for the IPv4 scan",
			addrs: []string{"not-an-ip", "192.168.0.159"},
			want:  "192.168.0.159",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := preferIPv4Addr(tc.addrs); got != tc.want {
				t.Errorf("preferIPv4Addr(%v) = %q, want %q", tc.addrs, got, tc.want)
			}
		})
	}
}

// fakeBrowseBackend is a scripted browseBackendFn double: the test pushes
// services onto ch and the backend hands them to whichever generic function
// is under test, staying alive until ctx ends. Mirrors stream_test.go's
// fakeBackend, but kept separate because browseBackendFn is its own seam.
type fakeBrowseBackend struct{ ch chan MDNSService }

func newFakeBrowseBackend() *fakeBrowseBackend { return &fakeBrowseBackend{ch: make(chan MDNSService)} }

func (f *fakeBrowseBackend) fn(ctx context.Context, _ string, emit func(MDNSService)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case svc := <-f.ch:
			emit(svc)
		}
	}
}

// emit pushes svc to the backend, failing the test if it is never picked up
// (a hung consumer would otherwise deadlock the test).
func (f *fakeBrowseBackend) emit(t *testing.T, svc MDNSService) {
	t.Helper()
	select {
	case f.ch <- svc:
	case <-time.After(2 * time.Second):
		t.Fatalf("backend emit blocked: %+v", svc)
	}
}

// useBrowseBackend points browseBackendFn at a fake backend for the duration
// of the test.
func useBrowseBackend(t *testing.T, backend func(context.Context, string, func(MDNSService)) error) {
	t.Helper()
	orig := browseBackendFn
	t.Cleanup(func() { browseBackendFn = orig })
	browseBackendFn = backend
}

// TestBrowseMDNSServicesContinuousForwardsUntilCancel pins the property the
// cutover changes on purpose: BrowseMDNSServicesContinuous no longer
// deduplicates (that was darwin-specific behavior baked into the per-platform
// implementation this replaces) — it forwards every emission from the
// backend as-is, matching mdnsStreamBackend's own no-dedup contract, and
// stops only when ctx is cancelled.
func TestBrowseMDNSServicesContinuousForwardsUntilCancel(t *testing.T) {
	fb := newFakeBrowseBackend()
	useBrowseBackend(t, fb.fn)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := BrowseMDNSServicesContinuous(ctx, "_wendyos._udp")
	if err != nil {
		t.Fatalf("BrowseMDNSServicesContinuous: %v", err)
	}

	svc := MDNSService{InstanceName: "orin", Hostname: "orin.local", Port: 50051}

	// The same instance twice: a real deduping implementation would collapse
	// this to one emission on the channel.
	go func() {
		fb.emit(t, svc)
		fb.emit(t, svc)
	}()

	for i := 0; i < 2; i++ {
		select {
		case got, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d of 2 emissions", i)
			}
			if got.InstanceName != svc.InstanceName || got.Hostname != svc.Hostname || got.Port != svc.Port {
				t.Errorf("emission %d = %+v, want %+v", i, got, svc)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for emission %d", i)
		}
	}

	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to close after ctx cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not close after ctx cancellation")
	}
}

// TestBrowseMDNSServicesSettlesEarlyAndDedups pins BrowseMDNSServices'
// early-exit and dedup behavior: it must return browseSettle after the last
// *newly* discovered service — not after the last emission of any kind — and
// it must collapse a repeated announcement of the same instance
// (InstanceName+Hostname+Port) into a single result.
//
// The settle timer must only be pushed out by a new service: mdnsStreamBackend
// deliberately does not dedup (a multi-homed device or an ordinary
// re-announcement re-fires Add, per its own doc comment), so a version that
// re-arms on every emission — new or repeat — would have a duplicate alone
// keep resetting the clock, defeating the early exit this behavior exists
// for. This test proves that by continuing to emit a duplicate of an
// already-seen instance after the last new service and asserting the call
// still returns close to a single browseSettle window later, not one
// stretched out by those repeats.
func TestBrowseMDNSServicesSettlesEarlyAndDedups(t *testing.T) {
	shrinkDuration(t, &browseSettle, 90*time.Millisecond)

	fb := newFakeBrowseBackend()
	useBrowseBackend(t, fb.fn)

	orin := MDNSService{InstanceName: "orin", Hostname: "orin.local", Port: 50051}
	nano := MDNSService{InstanceName: "nano", Hostname: "nano.local", Port: 50051}
	lastNewCh := make(chan time.Time, 1)

	go func() {
		fb.emit(t, orin)
		fb.emit(t, nano)
		lastNewCh <- time.Now()

		// Keep re-announcing an already-seen instance roughly every 1/3 of a
		// settle window. If a repeat re-armed settle the way a new service
		// does, this loop alone would stall BrowseMDNSServices well past a
		// single browseSettle window; each send attempt gives up as soon as
		// the backend stops reading, which happens the moment settle
		// actually fires.
		interval := browseSettle / 3
		for i := 0; i < 6; i++ {
			select {
			case fb.ch <- orin:
			case <-time.After(interval):
				return // nobody's reading anymore — settle already fired
			}
			time.Sleep(interval)
		}
	}()

	got, err := BrowseMDNSServices(context.Background(), "_wendyos._udp", 5*time.Second)
	settledAfter := time.Since(<-lastNewCh)
	if err != nil {
		t.Fatalf("BrowseMDNSServices error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d services, want 2 (deduped): %+v", len(got), got)
	}
	// Correct behavior returns browseSettle after the last new service
	// (nano) regardless of the duplicate re-announcements that follow. A
	// version that re-arms on every emission would instead be walked
	// forward by each repeat, landing well past this margin.
	if want := 2 * browseSettle; settledAfter > want {
		t.Fatalf("BrowseMDNSServices settled %v after the last new service, want <= %v — duplicate re-announcements appear to be re-arming the settle timer", settledAfter, want)
	}
}

// TestBrowseMDNSServicesTimeoutCap pins the other half of BrowseMDNSServices'
// early-exit policy: when nothing is ever found, it must still return once
// timeout elapses rather than blocking forever waiting for a settle that will
// never come.
func TestBrowseMDNSServicesTimeoutCap(t *testing.T) {
	fb := newFakeBrowseBackend() // stays silent: nothing on the network
	useBrowseBackend(t, fb.fn)

	const timeout = 100 * time.Millisecond
	start := time.Now()
	got, err := BrowseMDNSServices(context.Background(), "_wendyos._udp", timeout)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("BrowseMDNSServices error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d services, want 0: %+v", len(got), got)
	}
	if elapsed < timeout {
		t.Fatalf("returned after %v, want >= the %v timeout", elapsed, timeout)
	}
	if elapsed > timeout+time.Second {
		t.Fatalf("returned after %v, want close to the %v timeout", elapsed, timeout)
	}
}

// A VM's agent advertises its board in a devicetype TXT record, so a sighting
// can be classified before -- or instead of -- an agent probe: a user-mode VM's
// announcement leaks onto the LAN with an address nothing can reach.
func TestLANDeviceFromServiceReadsDeviceTypeTXTRecord(t *testing.T) {
	svc := MDNSService{Hostname: "wendyos-gentle-forest.local", TXTRecords: map[string]string{"devicetype": "vm-arm64"}}
	if got := lanDeviceFromService(svc).DeviceType; got != "vm-arm64" {
		t.Fatalf("DeviceType = %q, want vm-arm64 from the devicetype TXT record", got)
	}
	if got := lanDeviceFromService(MDNSService{Hostname: "orin.local"}).DeviceType; got != "" {
		t.Fatalf("DeviceType = %q with no devicetype record, want empty", got)
	}
}
