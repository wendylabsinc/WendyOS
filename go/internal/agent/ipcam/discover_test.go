package ipcam

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestHostFromXAddrs(t *testing.T) {
	cases := []struct {
		name, want string
		in         []string
	}{
		{"typical", "10.98.0.50", []string{"http://10.98.0.50/onvif/device_service"}},
		{"with port", "10.98.0.50", []string{"http://10.98.0.50:8000/onvif/device_service"}},
		{"first wins", "10.98.0.50", []string{"http://10.98.0.50/x", "http://192.168.0.9/x"}},
		{"skips unusable", "192.168.0.9", []string{"::::", "http://192.168.0.9/x"}},
		{"empty", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HostFromXAddrs(tc.in); got != tc.want {
				t.Fatalf("HostFromXAddrs(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

const procNetARP = `IP address       HW type     Flags       HW address            Mask     Device
10.98.0.50       0x1         0x2         ec:71:db:2a:ae:7e     *        eth0
192.168.0.1      0x1         0x2         a8:9c:6c:92:a7:35     *        wlan0
10.98.0.99       0x1         0x0         00:00:00:00:00:00     *        eth0
`

func TestMACFromARP(t *testing.T) {
	if got := MACFromARP(procNetARP, "10.98.0.50"); got != "ec:71:db:2a:ae:7e" {
		t.Fatalf("MACFromARP = %q, want ec:71:db:2a:ae:7e", got)
	}
	// An incomplete entry carries flags 0x0 and an all-zero address; that is not
	// an identity we can key a registry on.
	if got := MACFromARP(procNetARP, "10.98.0.99"); got != "" {
		t.Fatalf("MACFromARP for incomplete entry = %q, want empty", got)
	}
	if got := MACFromARP(procNetARP, "10.98.0.7"); got != "" {
		t.Fatalf("MACFromARP for unknown ip = %q, want empty", got)
	}
	if got := MACFromARP("", "10.98.0.50"); got != "" {
		t.Fatalf("MACFromARP on empty table = %q, want empty", got)
	}
}

func newTestDiscoverer(t *testing.T, payloads [][]byte, arp string) (*Discoverer, *Registry) {
	t.Helper()
	reg := NewRegistry(filepath.Join(t.TempDir(), "cameras.json"))
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	d := NewDiscoverer(reg, zap.NewNop())
	d.probe = func(ctx context.Context) ([][]byte, error) { return payloads, nil }
	d.arpTable = func() (string, error) { return arp, nil }
	return d, reg
}

// Once must turn probe responses into registry entries, keyed by the MAC found in
// the ARP table.
func TestOnceRegistersDiscoveredCamera(t *testing.T) {
	d, reg := newTestDiscoverer(t, [][]byte{[]byte(reolinkProbeMatch)}, procNetARP)

	found, err := d.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d cameras, want 1", len(found))
	}
	if found[0].MAC != "ec:71:db:2a:ae:7e" {
		t.Fatalf("MAC = %q", found[0].MAC)
	}
	if found[0].Model != "RLC-520A" {
		t.Fatalf("Model = %q", found[0].Model)
	}
	if found[0].Address != "10.98.0.50" {
		t.Fatalf("Address = %q", found[0].Address)
	}
	if found[0].ID != IDBandStart {
		t.Fatalf("ID = %d, want %d", found[0].ID, IDBandStart)
	}
	if got := len(reg.List()); got != 1 {
		t.Fatalf("registry has %d cameras, want 1", got)
	}
}

// Probing twice must not create a second entry for the same camera.
func TestOnceIsIdempotent(t *testing.T) {
	d, reg := newTestDiscoverer(t, [][]byte{[]byte(reolinkProbeMatch)}, procNetARP)
	if _, err := d.Once(context.Background()); err != nil {
		t.Fatalf("first Once: %v", err)
	}
	if _, err := d.Once(context.Background()); err != nil {
		t.Fatalf("second Once: %v", err)
	}
	if got := len(reg.List()); got != 1 {
		t.Fatalf("registry has %d cameras after two probes, want 1", got)
	}
}

// A camera whose MAC cannot be resolved is skipped rather than registered under
// an unstable key, since the registry is MAC-keyed and would otherwise allocate a
// fresh ID on every round.
func TestOnceSkipsCameraWithoutMAC(t *testing.T) {
	d, reg := newTestDiscoverer(t, [][]byte{[]byte(reolinkProbeMatch)}, "")

	found, err := d.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("found %d cameras, want 0", len(found))
	}
	if got := len(reg.List()); got != 0 {
		t.Fatalf("registry has %d cameras, want 0", got)
	}
}

// Unrelated traffic on port 3702 must not abort a probe round.
func TestOnceIgnoresNonProbeResponses(t *testing.T) {
	d, _ := newTestDiscoverer(t,
		[][]byte{[]byte("garbage"), []byte(reolinkProbeMatch), []byte("<Envelope/>")}, procNetARP)

	found, err := d.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d cameras, want 1", len(found))
	}
}

// Two replies from one camera, which multicast makes common, must not double it.
func TestOnceDeduplicatesWithinRound(t *testing.T) {
	d, reg := newTestDiscoverer(t,
		[][]byte{[]byte(reolinkProbeMatch), []byte(reolinkProbeMatch)}, procNetARP)

	found, err := d.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d cameras, want 1", len(found))
	}
	if got := len(reg.List()); got != 1 {
		t.Fatalf("registry has %d cameras, want 1", got)
	}
}

// A probe failure is reported, not swallowed: the caller decides whether a round
// that could not run matters.
func TestOnceReturnsProbeError(t *testing.T) {
	d, _ := newTestDiscoverer(t, nil, procNetARP)
	sentinel := errors.New("socket unavailable")
	d.probe = func(ctx context.Context) ([][]byte, error) { return nil, sentinel }
	if _, err := d.Once(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the probe error", err)
	}
}

// An unreadable ARP table degrades to finding nothing, rather than failing the
// round: /proc/net/arp is absent on non-Linux hosts.
func TestOnceToleratesARPFailure(t *testing.T) {
	d, _ := newTestDiscoverer(t, [][]byte{[]byte(reolinkProbeMatch)}, "")
	d.arpTable = func() (string, error) { return "", errors.New("no /proc") }
	found, err := d.Once(context.Background())
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("found %d cameras, want 0", len(found))
	}
}

// A probe must go out of every interface, not just the default route: a camera on
// its own link is unreachable through the uplink, which is the whole setup this
// package exists for.
func TestMulticastProbeIsPerInterface(t *testing.T) {
	d, _ := newTestDiscoverer(t, nil, procNetARP)
	d.localIPv4s = func() []string { return []string{"192.168.0.100", "10.98.0.1"} }

	var mu sync.Mutex
	var sources []string
	d.probeOne = func(source string, _ time.Time) ([][]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		sources = append(sources, source)
		return [][]byte{[]byte(source)}, nil
	}

	got, err := d.multicastProbe(context.Background())
	if err != nil {
		t.Fatalf("multicastProbe: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	sort.Strings(sources)
	if len(sources) != 2 || sources[0] != "10.98.0.1" || sources[1] != "192.168.0.100" {
		t.Fatalf("probed %v, want one probe per local address", sources)
	}
	// Replies from every interface must be merged, not just the last one's.
	if len(got) != 2 {
		t.Fatalf("collected %d payloads, want 2", len(got))
	}
}

// One link failing mid-probe must not discard what the others found.
func TestMulticastProbeKeepsRepliesWhenOneSourceFails(t *testing.T) {
	d, _ := newTestDiscoverer(t, nil, procNetARP)
	d.localIPv4s = func() []string { return []string{"192.168.0.100", "10.98.0.1"} }
	d.probeOne = func(source string, _ time.Time) ([][]byte, error) {
		if source == "10.98.0.1" {
			return nil, errors.New("link went down")
		}
		return [][]byte{[]byte(reolinkProbeMatch)}, nil
	}

	got, err := d.multicastProbe(context.Background())
	if err != nil {
		t.Fatalf("multicastProbe: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("collected %d payloads, want the surviving one", len(got))
	}
}

// If every source fails the error is surfaced rather than reported as an empty
// round, which would look like "no cameras here".
func TestMulticastProbeReportsTotalFailure(t *testing.T) {
	d, _ := newTestDiscoverer(t, nil, procNetARP)
	d.localIPv4s = func() []string { return []string{"192.168.0.100"} }
	sentinel := errors.New("no socket")
	d.probeOne = func(string, time.Time) ([][]byte, error) { return nil, sentinel }

	if _, err := d.multicastProbe(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the probe error", err)
	}
}

// With no addresses at all the probe still tries the default route rather than
// silently doing nothing.
func TestMulticastProbeFallsBackToDefaultRoute(t *testing.T) {
	d, _ := newTestDiscoverer(t, nil, procNetARP)
	d.localIPv4s = func() []string { return nil }
	var sources []string
	d.probeOne = func(source string, _ time.Time) ([][]byte, error) {
		sources = append(sources, source)
		return nil, nil
	}
	if _, err := d.multicastProbe(context.Background()); err != nil {
		t.Fatalf("multicastProbe: %v", err)
	}
	if len(sources) != 1 || sources[0] != "" {
		t.Fatalf("probed %v, want a single default-route probe", sources)
	}
}

func TestListLocalIPv4sSkipsLoopback(t *testing.T) {
	for _, addr := range listLocalIPv4s() {
		if addr == "127.0.0.1" {
			t.Fatal("loopback must not be probed")
		}
	}
}

// A camera holding a lease from an earlier session answers no probe, so liveness
// has to be checked directly or it would sit offline forever.
func TestOnceRefreshesLivenessOfKnownCamera(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "cameras.json"))
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	cam, err := reg.Upsert(Camera{MAC: "ec:71:db:2a:ae:7e", Address: "10.98.0.50"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Reload so the camera starts offline, the way it does after a restart.
	reloaded := NewRegistry(reg.path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, _ := reloaded.Get(cam.ID); got.Online {
		t.Fatal("camera loaded from disk should start offline")
	}

	d := NewDiscoverer(reloaded, zap.NewNop())
	d.probe = func(ctx context.Context) ([][]byte, error) { return nil, nil }
	d.arpTable = func() (string, error) { return "", nil }
	var dialed []string
	d.reachable = func(address string) bool {
		dialed = append(dialed, address)
		return true
	}

	if _, err := d.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(dialed) != 1 || dialed[0] != "10.98.0.50" {
		t.Fatalf("dialed %v, want the known camera address", dialed)
	}
	if got, _ := reloaded.Get(cam.ID); !got.Online {
		t.Fatal("a reachable known camera was not marked online")
	}
}

// A camera that has gone away must flip to offline so the listing is honest.
func TestOnceMarksUnreachableCameraOffline(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "cameras.json"))
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	cam, err := reg.Upsert(Camera{MAC: "ec:71:db:2a:ae:7e", Address: "10.98.0.50"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	d := NewDiscoverer(reg, zap.NewNop())
	d.probe = func(ctx context.Context) ([][]byte, error) { return nil, nil }
	d.arpTable = func() (string, error) { return "", nil }
	d.reachable = func(string) bool { return false }

	if _, err := d.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got, _ := reg.Get(cam.ID); got.Online {
		t.Fatal("an unreachable camera is still reported online")
	}
}

// A newly discovered camera is asked, over ONVIF, what its streams are called,
// and the answer lands in the registry so StreamURL stops guessing Reolink
// paths. Once the paths are known the camera is not asked again.
func TestOnceLearnsStreamPathsOnceAndKeepsThem(t *testing.T) {
	d, reg := newTestDiscoverer(t, [][]byte{[]byte(reolinkProbeMatch)}, procNetARP)
	var asked int
	d.resolveStreams = func(ctx context.Context, cam Camera, cred Credential) (StreamPaths, error) {
		asked++
		if cam.ONVIFAddr != "http://10.98.0.50/onvif/device_service" {
			t.Fatalf("resolver got ONVIFAddr %q", cam.ONVIFAddr)
		}
		return StreamPaths{Sub: "/media/live/1/2", Main: "/media/live/1/1"}, nil
	}

	for round := 0; round < 2; round++ {
		if _, err := d.Once(context.Background()); err != nil {
			t.Fatalf("Once round %d: %v", round, err)
		}
	}
	if asked != 1 {
		t.Fatalf("resolver asked %d times, want exactly once", asked)
	}
	cams := reg.List()
	if len(cams) != 1 || cams[0].StreamSub != "/media/live/1/2" || cams[0].StreamMain != "/media/live/1/1" {
		t.Fatalf("registry = %+v", cams)
	}
	if got := streamPath(cams[0], StreamAuto); got != "/media/live/1/2" {
		t.Fatalf("streamPath(auto) = %q, want the learned sub path, not the Reolink default", got)
	}
}

// A camera that will not say what its streams are is still registered, with
// the default paths as before, and is asked again next round.
func TestOnceKeepsCameraWhenStreamPathsAreUnknown(t *testing.T) {
	d, reg := newTestDiscoverer(t, [][]byte{[]byte(reolinkProbeMatch)}, procNetARP)
	var asked int
	d.resolveStreams = func(context.Context, Camera, Credential) (StreamPaths, error) {
		asked++
		return StreamPaths{}, ErrNoStreamPaths
	}
	for round := 0; round < 2; round++ {
		if _, err := d.Once(context.Background()); err != nil {
			t.Fatalf("Once: %v", err)
		}
	}
	if asked != 2 {
		t.Fatalf("resolver asked %d times, want once per round while unknown", asked)
	}
	cams := reg.List()
	if len(cams) != 1 || cams[0].StreamSub != "" {
		t.Fatalf("registry = %+v", cams)
	}
	if got := streamPath(cams[0], StreamAuto); got != defaultSubPath {
		t.Fatalf("streamPath(auto) = %q, want the default while unknown", got)
	}
}

// The stored login reaches the resolver, keyed by the camera's MAC.
func TestOnceHandsTheCamerasLoginToTheResolver(t *testing.T) {
	d, _ := newTestDiscoverer(t, [][]byte{[]byte(reolinkProbeMatch)}, procNetARP)
	d.Credentials = func(mac string) (Credential, bool) {
		if mac != "ec:71:db:2a:ae:7e" {
			return Credential{}, false
		}
		return Credential{Username: "admin", Password: "hunter2"}, true
	}
	var seen Credential
	d.resolveStreams = func(_ context.Context, _ Camera, cred Credential) (StreamPaths, error) {
		seen = cred
		return StreamPaths{Sub: "/s", Main: "/m"}, nil
	}
	if _, err := d.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if seen.Username != "admin" || seen.Password != "hunter2" {
		t.Fatalf("resolver saw credential %+v", seen)
	}
}
