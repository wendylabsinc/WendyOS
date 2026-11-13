package discovery

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// wiredService is a sighting of a device that advertises wired as its cable.
func wiredService(ip, iface, wired string) MDNSService {
	svc := wendyService("dev-wired", "pi", "pi.local", ip, 50051)
	svc.InterfaceName = iface
	if wired != "" {
		svc.TXTRecords["wired"] = wired
	}
	return svc
}

func TestLANDeviceFromServiceReadsWiredTXTRecord(t *testing.T) {
	svc := MDNSService{Hostname: "pi.local", TXTRecords: map[string]string{"wired": "10.42.0.120, 10.42.1.9,not-an-ip"}}

	if got, want := lanDeviceFromService(svc).WiredAddresses, []string{"10.42.0.120", "10.42.1.9"}; !slices.Equal(got, want) {
		t.Errorf("WiredAddresses = %v, want %v", got, want)
	}
	if got := lanDeviceFromService(MDNSService{Hostname: "pi.local"}).WiredAddresses; got != nil {
		t.Errorf("WiredAddresses = %v without a wired record, want nil", got)
	}
}

// A record names private addresses that may mean another device to this host;
// only a cable the device was heard answering from counts.
func TestStreamKeepsOnlyCablesTheDeviceAnsweredFrom(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)
	events, _ := startStream(t, StreamOptions{})

	fb.emit(t, wiredService("192.168.2.216", "en0", "192.168.2.50"))
	if got := collectEvents(t, events, 1, 5*time.Second)[0].Device.WiredAddresses; got != nil {
		t.Fatalf("WiredAddresses = %v before the cable answered, want nil", got)
	}
	fb.emit(t, wiredService("192.168.2.50", "en0", "192.168.2.50"))

	ev := collectEvents(t, events, 1, 5*time.Second)[0]
	if ev.Kind != LANUpdated || !slices.Equal(ev.Device.WiredAddresses, []string{"192.168.2.50"}) {
		t.Errorf("answered cable not reported: %+v", ev)
	}
}

// One host link hearing a device's cable and Wi-Fi often caches only one of
// them. The cable counts once it answers for itself, and the cache keeps it.
func TestStreamCountsACableThatAnswers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))
	useCables(t, []string{"192.168.2.50"}, []string{"192.168.2.50"})
	events, stop := startStream(t, StreamOptions{UseCache: true})

	fb.emit(t, wiredService("192.168.2.216", "en0", "192.168.2.50"))

	got := collectEvents(t, events, 2, 5*time.Second)
	if got[0].Kind != LANFound || got[0].Device.WiredAddresses != nil {
		t.Fatalf("first event = %+v, want the sighting without an unproven cable", got[0])
	}
	if got[1].Kind != LANUpdated || !slices.Equal(got[1].Device.WiredAddresses, []string{"192.168.2.50"}) {
		t.Fatalf("second event = %+v, want the cable that answered", got[1])
	}
	stop()
	c, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if fresh := c.Fresh(time.Now()); len(fresh) != 1 || !slices.Equal(fresh[0].WiredAddresses, []string{"192.168.2.50"}) {
		t.Errorf("cache = %+v, want the answered cable kept", fresh)
	}
}

// Re-announcements must not repeat the question; a silent cable changes
// nothing.
func TestStreamAsksACableOnce(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)
	asked := useCables(t, []string{"192.168.2.50"}, nil)
	events, _ := startStream(t, StreamOptions{})

	for range 3 {
		fb.emit(t, wiredService("192.168.2.216", "en0", "192.168.2.50"))
	}

	collectEvents(t, events, 1, 5*time.Second)
	expectQuiet(t, events, 200*time.Millisecond)
	if got := asked.all(); !slices.Equal(got, []string{"192.168.2.50"}) {
		t.Errorf("asked %v, want the cable once", got)
	}
}

// The record can arrive with a later announcement, as when the cable comes up
// while the session runs.
func TestStreamAsksACableAdvertisedLater(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)
	useCables(t, []string{"192.168.2.50"}, []string{"192.168.2.50"})
	events, _ := startStream(t, StreamOptions{})

	fb.emit(t, wiredService("192.168.2.216", "en0", ""))
	collectEvents(t, events, 1, 5*time.Second)
	fb.emit(t, wiredService("192.168.2.216", "en0", "192.168.2.50"))

	ev := collectEvents(t, events, 1, 5*time.Second)[0]
	if ev.Kind != LANUpdated || !slices.Equal(ev.Device.WiredAddresses, []string{"192.168.2.50"}) {
		t.Errorf("event = %+v, want the cable that answered", ev)
	}
}

// A cable the device already answered from needs no question, and one off this
// host's links cannot be asked.
func TestStreamAsksOnlyUnheardCablesOnThisHostsLinks(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)
	asked := useCables(t, []string{"192.168.2.50"}, []string{"192.168.2.50"})
	events, _ := startStream(t, StreamOptions{})

	fb.emit(t, wiredService("192.168.2.50", "en0", "192.168.2.50,10.9.0.4"))

	ev := collectEvents(t, events, 1, 5*time.Second)[0]
	expectQuiet(t, events, 200*time.Millisecond)
	if !slices.Equal(ev.Device.WiredAddresses, []string{"192.168.2.50"}) || len(asked.all()) > 0 {
		t.Errorf("WiredAddresses = %v after asking %v, want the heard cable unasked", ev.Device.WiredAddresses, asked.all())
	}
}

// The connect path asks a cable before dialling it, so a cable an earlier
// session heard stays until this one asks it and it stays silent.
func TestStreamKeepsACachedCableUntilItStaysSilent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	seedCache(t, path, discoverycache.Entry{ID: "dev-wired", DisplayName: "pi", Hostname: "pi.local",
		IP: "192.168.2.216", Port: 50051, WiredAddresses: []string{"192.168.2.50"}})
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))
	useCables(t, []string{"192.168.2.50"}, nil)
	events, stop := startStream(t, StreamOptions{UseCache: true})
	collectEvents(t, events, 1, 5*time.Second) // the cached row

	fb.emit(t, wiredService("192.168.2.216", "en0", "192.168.2.50"))

	got := collectEvents(t, events, 2, 5*time.Second)
	if got[0].Kind != LANFound || !slices.Equal(got[0].Device.WiredAddresses, []string{"192.168.2.50"}) {
		t.Fatalf("sighting = %+v, want the cached cable kept while it is asked", got[0])
	}
	if got[1].Kind != LANUpdated || got[1].Device.WiredAddresses != nil {
		t.Fatalf("answer = %+v, want the silent cable dropped", got[1])
	}
	stop()
	c, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if fresh := c.Fresh(time.Now()); len(fresh) != 1 || fresh[0].WiredAddresses != nil {
		t.Errorf("cache = %+v, want the silent cable gone", fresh)
	}
}

// A cached cable that cannot be asked stays; one the device stopped
// advertising goes.
func TestStreamKeepsACachedCableWhileAdvertised(t *testing.T) {
	for _, tc := range []struct {
		name, wired string
		want        []string
	}{
		{"still advertised", "192.168.2.50", []string{"192.168.2.50"}},
		{"withdrawn", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "devices.json")
			seedCache(t, path, discoverycache.Entry{ID: "dev-wired", DisplayName: "pi", Hostname: "pi.local",
				IP: "192.168.2.216", Port: 50051, WiredAddresses: []string{"192.168.2.50"}})
			fb := newFakeBackend()
			useStreamSeams(t, fb.fn, cacheLoaderFor(path))
			events, _ := startStream(t, StreamOptions{UseCache: true})
			collectEvents(t, events, 1, 5*time.Second)

			fb.emit(t, wiredService("192.168.2.216", "en0", tc.wired))

			if got := collectEvents(t, events, 1, 5*time.Second)[0].Device.WiredAddresses; !slices.Equal(got, tc.want) {
				t.Errorf("WiredAddresses = %v, want %v", got, tc.want)
			}
			expectQuiet(t, events, 200*time.Millisecond)
		})
	}
}

// A cached device confirmed by its probe may never be sighted; its cable is
// still asked, but only once the device is known to be up.
func TestStreamAsksTheCablesOfADeviceItsProbeConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prober LANProber
		asks   int
	}{
		{"probe answered", func(_ context.Context, dev models.LANDevice) (models.LANDevice, error) { return dev, nil }, 1},
		{"probe failed", failingProber(errors.New("connection refused")), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "devices.json")
			seedCache(t, path, discoverycache.Entry{ID: "dev-wired", DisplayName: "pi", Hostname: "pi.local",
				IP: "192.168.2.216", Port: 50051, WiredAddresses: []string{"192.168.2.50"}})
			fb := newFakeBackend() // never sights the device
			useStreamSeams(t, fb.fn, cacheLoaderFor(path))
			asked := useCables(t, []string{"192.168.2.50"}, nil)
			events, _ := startStream(t, StreamOptions{UseCache: true, Prober: tc.prober})
			collectEvents(t, events, 1, 5*time.Second) // the cached row

			if tc.asks > 0 {
				got := collectEvents(t, events, 2, 5*time.Second)
				if got[1].Kind != LANUpdated || got[1].Device.WiredAddresses != nil {
					t.Errorf("events = %+v, want the silent cable dropped after the probe", got)
				}
			} else {
				expectQuiet(t, events, 300*time.Millisecond)
			}
			if n := len(asked.all()); n != tc.asks {
				t.Errorf("asked %d times, want %d", n, tc.asks)
			}
		})
	}
}

// Asking can take a while; it must not hold the session open, nor an answer
// that lands after its device is gone hurt.
func TestStreamEndsWithAnAskInFlight(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)
	useCables(t, []string{"192.168.2.50"}, nil)
	asking := make(chan struct{})
	cableAnswersFn = func(ctx context.Context, _, _ string) bool {
		close(asking)
		<-ctx.Done()
		return false
	}
	events, stop := startStream(t, StreamOptions{})
	fb.emit(t, wiredService("192.168.2.216", "en0", "192.168.2.50"))
	collectEvents(t, events, 1, 5*time.Second)
	<-asking

	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the session did not end with an ask in flight")
	}
}

func TestHandleCableAnswerForAGoneDevice(t *testing.T) {
	s := &lanStream{ctx: context.Background(), states: map[string]*lanDeviceState{}}
	s.handleCableAnswer(cableAnswer{key: "gone", addr: "192.168.2.50", answered: true})
}

// An ask the ending session cut short is no evidence of silence.
func TestHandleCableAnswerIgnoresACutShortAsk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := &lanDeviceState{advertised: []string{"192.168.2.50"}, cached: []string{"192.168.2.50"}}
	st.dev.WiredAddresses = st.cables()
	s := &lanStream{ctx: ctx, states: map[string]*lanDeviceState{"pi": st}}

	s.handleCableAnswer(cableAnswer{key: "pi", addr: "192.168.2.50"})

	if !slices.Equal(st.dev.WiredAddresses, []string{"192.168.2.50"}) {
		t.Errorf("WiredAddresses = %v, want the cached cable kept", st.dev.WiredAddresses)
	}
}

// The device withdrew the cable while it was being asked.
func TestStreamDropsACableWithdrawnWhileAsked(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)
	useCables(t, []string{"192.168.2.50"}, nil)
	release := make(chan struct{})
	cableAnswersFn = func(ctx context.Context, _, _ string) bool {
		select {
		case <-release:
			return true
		case <-ctx.Done():
			return false
		}
	}
	events, _ := startStream(t, StreamOptions{})
	fb.emit(t, wiredService("192.168.2.216", "en0", "192.168.2.50"))
	collectEvents(t, events, 1, 5*time.Second)
	fb.emit(t, wiredService("192.168.2.217", "en0", "")) // moved too: its event shows the engine took it
	collectEvents(t, events, 1, 5*time.Second)

	close(release)
	expectQuiet(t, events, 200*time.Millisecond)
}

// A sighting without a hostname gives nothing to ask for; the cable is asked
// once one arrives.
func TestStreamWaitsForAHostnameBeforeAsking(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)
	useCables(t, []string{"192.168.2.50"}, nil)
	hosts := make(chan string, 4)
	cableAnswersFn = func(_ context.Context, _, host string) bool {
		hosts <- host
		return false
	}
	events, _ := startStream(t, StreamOptions{})
	nameless := wiredService("192.168.2.216", "en0", "192.168.2.50")
	nameless.Hostname = ""
	fb.emit(t, nameless)
	collectEvents(t, events, 1, 5*time.Second)
	fb.emit(t, wiredService("192.168.2.216", "en0", "192.168.2.50"))

	select {
	case host := <-hosts:
		if host != "pi.local" {
			t.Errorf("asked for %q, want pi.local", host)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cable was never asked")
	}
}

// A cable answering after the probe failed must not hide that failure.
func TestStreamKeepsAProbeFailureThroughACableAnswer(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)
	useCables(t, []string{"192.168.2.50"}, nil)
	release := make(chan struct{})
	cableAnswersFn = func(ctx context.Context, _, _ string) bool {
		select {
		case <-release:
			return true
		case <-ctx.Done():
			return false
		}
	}
	events, _ := startStream(t, StreamOptions{Prober: failingProber(errors.New("connection refused"))})
	fb.emit(t, wiredService("192.168.2.216", "en0", "192.168.2.50"))
	if got := collectEvents(t, events, 2, 5*time.Second)[1]; !got.ProbeFailed {
		t.Fatalf("event = %+v, want the probe failure", got)
	}

	close(release)

	ev := collectEvents(t, events, 1, 5*time.Second)[0]
	if !ev.ProbeFailed || ev.Probed || !slices.Equal(ev.Device.WiredAddresses, []string{"192.168.2.50"}) {
		t.Errorf("event = %+v, want the cable with the probe failure kept", ev)
	}
}

// Several silent cables must not delay the one that answers.
func TestStreamAsksCablesTogether(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)
	cables := []string{"192.168.2.50", "192.168.2.51", "192.168.2.52"}
	useCables(t, cables, nil)
	var inFlight, most atomic.Int32
	cableAnswersFn = func(context.Context, string, string) bool {
		n := inFlight.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		return false
	}
	events, _ := startStream(t, StreamOptions{})

	fb.emit(t, wiredService("192.168.2.216", "en0", strings.Join(cables, ",")))

	collectEvents(t, events, 1, 5*time.Second)
	expectQuiet(t, events, 300*time.Millisecond)
	if got := most.Load(); got != int32(len(cables)) {
		t.Errorf("at most %d asks at once, want %d", got, len(cables))
	}
}

// A changed record must reach subscribers and the cache, which the connect
// path reads it from.
func TestStreamReportsAChangedWiredRecord(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)
	events, _ := startStream(t, StreamOptions{})

	fb.emit(t, wiredService("10.9.0.4", "en0", "10.9.0.4"))
	collectEvents(t, events, 1, 5*time.Second)
	fb.emit(t, wiredService("10.9.0.7", "en0", "10.9.0.7"))

	ev := collectEvents(t, events, 1, 5*time.Second)[0]
	if ev.Kind != LANUpdated || !slices.Equal(ev.Device.WiredAddresses, []string{"10.9.0.7"}) {
		t.Errorf("changed wired record not reported: %+v", ev)
	}
}

// Cable out: the device drops the record, and the row, which the cache stores
// whole, must drop it too.
func TestStreamDropsAWithdrawnWiredRecord(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)
	events, _ := startStream(t, StreamOptions{})

	fb.emit(t, wiredService("10.9.0.4", "en0", "10.9.0.4"))
	collectEvents(t, events, 1, 5*time.Second)
	fb.emit(t, wiredService("10.9.0.4", "en0", ""))

	if got := collectEvents(t, events, 1, 5*time.Second)[0].Device.WiredAddresses; got != nil {
		t.Errorf("WiredAddresses = %v after the device withdrew them, want nil", got)
	}
}

// A device already listed must not pick up a cable it never answered from.
func TestStreamIgnoresAnUnansweredCableOnAKnownDevice(t *testing.T) {
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)
	events, _ := startStream(t, StreamOptions{})

	fb.emit(t, wiredService("192.168.2.216", "en0", ""))
	collectEvents(t, events, 1, 5*time.Second)
	fb.emit(t, wiredService("192.168.2.216", "en1", "192.168.2.50"))

	if got := collectEvents(t, events, 1, 5*time.Second)[0].Device.WiredAddresses; got != nil {
		t.Errorf("WiredAddresses = %v for a cable the device never answered from, want nil", got)
	}
}

// askLog records the addresses asked, from whichever goroutine asks.
type askLog struct {
	mu    sync.Mutex
	addrs []string
}

func (l *askLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.addrs)
}

// useCables puts exactly onLink on this host's links and makes exactly
// answering answer mDNS as pi.local.
func useCables(t *testing.T, onLink, answering []string) *askLog {
	t.Helper()
	origOnLink, origAnswers := onLinkFn, cableAnswersFn
	t.Cleanup(func() { onLinkFn, cableAnswersFn = origOnLink, origAnswers })
	asked := &askLog{}
	onLinkFn = func(addr string) bool { return slices.Contains(onLink, addr) }
	cableAnswersFn = func(_ context.Context, addr, host string) bool {
		asked.mu.Lock()
		asked.addrs = append(asked.addrs, addr)
		asked.mu.Unlock()
		return host == "pi.local" && slices.Contains(answering, addr)
	}
	return asked
}

// useResponder stands in for the mDNS responder CableAnswers asks, replying
// to each query with whatever reply returns; it counts the queries.
func useResponder(t *testing.T, reply func(q dnsmessage.Message) [][]byte) *atomic.Int32 {
	t.Helper()
	var queries atomic.Int32
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	orig := mdnsPort
	t.Cleanup(func() { mdnsPort = orig })
	mdnsPort = uint16(pc.LocalAddr().(*net.UDPAddr).Port)
	go func() {
		buf := make([]byte, 9000)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var q dnsmessage.Message
			if q.Unpack(buf[:n]) != nil {
				continue
			}
			queries.Add(1)
			for _, r := range reply(q) {
				_, _ = pc.WriteTo(r, from)
			}
		}
	}()
	return &queries
}

func aReply(t *testing.T, id uint16, name string, a [4]byte) []byte {
	t.Helper()
	b, err := (&dnsmessage.Message{
		Header: dnsmessage.Header{ID: id, Response: true, Authoritative: true},
		Answers: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 10},
			Body:   &dnsmessage.AResource{A: a},
		}},
	}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCableAnswers(t *testing.T) {
	loopback := [4]byte{127, 0, 0, 1}
	for _, tc := range []struct {
		name  string
		reply func(q dnsmessage.Message) [][]byte
		want  bool
	}{
		{"answers as the device", func(q dnsmessage.Message) [][]byte {
			return [][]byte{aReply(t, q.ID, "Pi.local.", loopback)}
		}, true},
		{"after a reply to an earlier query", func(q dnsmessage.Message) [][]byte {
			return [][]byte{aReply(t, q.ID+1, "pi.local.", [4]byte{10, 0, 0, 9}), aReply(t, q.ID, "pi.local.", loopback)}
		}, true},
		{"only a reply to another query", func(q dnsmessage.Message) [][]byte {
			return [][]byte{aReply(t, q.ID+1, "pi.local.", loopback)}
		}, false},
		{"not a reply", func(q dnsmessage.Message) [][]byte {
			r := aReply(t, q.ID, "pi.local.", loopback)
			r[2] &^= 0x80 // clear QR
			return [][]byte{r}
		}, false},
		{"another device", func(q dnsmessage.Message) [][]byte {
			return [][]byte{aReply(t, q.ID, "pi-2.local.", loopback)}
		}, false},
		{"another address", func(q dnsmessage.Message) [][]byte {
			return [][]byte{aReply(t, q.ID, "pi.local.", [4]byte{10, 0, 0, 9})}
		}, false},
		{"silent", func(dnsmessage.Message) [][]byte { return nil }, false},
		{"after a lost query", lossyFirst(func(q dnsmessage.Message) [][]byte {
			return [][]byte{aReply(t, q.ID, "pi.local.", loopback)}
		}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useResponder(t, tc.reply)
			start := time.Now()
			if got := CableAnswers(context.Background(), "127.0.0.1", "pi.local"); got != tc.want {
				t.Errorf("CableAnswers = %v, want %v", got, tc.want)
			}
			if elapsed := time.Since(start); elapsed > 2*cableAnswerTimeout {
				t.Errorf("took %v, want at most about %v", elapsed, cableAnswerTimeout)
			}
		})
	}
}

// A session that ends must not wait out a silent cable.
func TestCableAnswersStopsWhenCancelled(t *testing.T) {
	useResponder(t, func(dnsmessage.Message) [][]byte { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	start := time.Now()
	if CableAnswers(ctx, "127.0.0.1", "pi.local") || time.Since(start) > cableAnswerTimeout/2 {
		t.Errorf("CableAnswers ran %v after cancellation", time.Since(start))
	}
}

// Only an IPv4 address and a name can be asked; anything else sends nothing.
func TestCableAnswersNeedsAnIPv4AddressAndAName(t *testing.T) {
	queries := useResponder(t, func(q dnsmessage.Message) [][]byte {
		return [][]byte{aReply(t, q.ID, ".", [4]byte{127, 0, 0, 1})}
	})
	for _, tc := range [][2]string{{"", "pi.local"}, {"pi.local", "pi.local"}, {"::1", "pi.local"}, {"::ffff:127.0.0.1", "pi.local"}, {"127.0.0.1", ""}, {"127.0.0.1", "."}} {
		if CableAnswers(context.Background(), tc[0], tc[1]) {
			t.Errorf("CableAnswers(%q, %q) = true", tc[0], tc[1])
		}
	}
	if n := queries.Load(); n != 0 {
		t.Errorf("sent %d queries, want none", n)
	}
}

// lossyFirst drops the first query, as a lossy link would.
func lossyFirst(reply func(dnsmessage.Message) [][]byte) func(dnsmessage.Message) [][]byte {
	var seen atomic.Bool
	return func(q dnsmessage.Message) [][]byte {
		if !seen.Swap(true) {
			return nil
		}
		return reply(q)
	}
}

func TestOnLinkRecognisesThisHostsSubnets(t *testing.T) {
	for _, addr := range []string{"", "not-an-ip", "169.254.10.20", "fe80::1%eth0", "127.0.0.1", "127.0.0.2"} {
		if OnLink(addr) {
			t.Errorf("OnLink(%q) = true, want false", addr)
		}
	}
	neighbour := subnetNeighbour(t)
	if neighbour == "" {
		t.Skip("no IPv4 subnet on this host")
	}
	if !OnLink(neighbour) {
		t.Errorf("OnLink(%s) = false for an address on this host's own subnet", neighbour)
	}
}

// subnetNeighbour returns another address in one of this host's IPv4 subnets.
func subnetNeighbour(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ones, _ := ipn.Mask.Size(); ip4 == nil || ip4.IsLinkLocalUnicast() || ones > 30 {
				continue
			}
			neighbour := make(net.IP, 4)
			copy(neighbour, ip4)
			neighbour[3] ^= 1
			if !ipn.Contains(neighbour) || neighbour.Equal(ip4) {
				continue
			}
			return neighbour.String()
		}
	}
	return ""
}
