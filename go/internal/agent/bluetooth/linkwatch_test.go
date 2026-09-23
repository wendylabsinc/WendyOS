package bluetooth

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestParseHIDSupervisionTimeout(t *testing.T) {
	tests := []struct {
		raw     string
		want    uint16
		wantErr bool
	}{
		{"", 50, false},
		{"0", 0, false},
		{"500", 50, false},
		{" 750 ", 75, false},
		{"105", 10, false}, // rounded down to a 10 ms step
		{"100", 10, false},
		{"32000", 3200, false},
		{"99", 50, true},
		{"32001", 50, true},
		{"-5", 50, true},
		{"half a second", 50, true},
	}
	for _, tt := range tests {
		got, err := parseHIDSupervisionTimeout(tt.raw)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("parseHIDSupervisionTimeout(%q) = %d, %v; want %d, error %v", tt.raw, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestDesiredTimeout(t *testing.T) {
	tests := []struct {
		name                      string
		target, interval, latency uint16
		want                      uint16
	}{
		{"xbox pad: 7.5 ms, latency 0", 50, 6, 0, 50},
		{"floor above target: 100 ms, latency 4", 50, 80, 4, 101},
		{"floor exactly on a step moves past it", 1, 8, 0, 10}, // 20 ms floor → 30 ms, then raised to the 100 ms minimum
		{"target below the specification minimum", 1, 6, 0, 10},
		{"floor above the specification maximum", 50, 3200, 499, 3200},
	}
	for _, tt := range tests {
		if got := desiredTimeout(tt.target, tt.interval, tt.latency); got != tt.want {
			t.Errorf("%s: desiredTimeout(%d, %d, %d) = %d; want %d", tt.name, tt.target, tt.interval, tt.latency, got, tt.want)
		}
	}
}

func TestParseStoredConnParams(t *testing.T) {
	const info = "[General]\nName=Xbox Wireless Controller\n\n" +
		"[ConnectionParameters]\nMinInterval=6\nMaxInterval=9\nLatency=0\nTimeout=300\n\n[LongTermKey]\nKey=00\n"
	got, ok := parseStoredConnParams(info)
	want := connUpdate{IntervalMin: 6, IntervalMax: 9, Latency: 0, Timeout: 300}
	if !ok || got != want {
		t.Errorf("parseStoredConnParams = %+v, %v; want %+v, true", got, ok, want)
	}
	for name, s := range map[string]string{
		"no group":           "[General]\nName=Pad\n",
		"missing timeout":    "[ConnectionParameters]\nMinInterval=6\nMaxInterval=6\nLatency=0\n",
		"key in other group": "[ConnectionParameters]\nMinInterval=6\nMaxInterval=6\nLatency=0\n[General]\nTimeout=300\n",
	} {
		if _, ok := parseStoredConnParams(s); ok {
			t.Errorf("%s: parseStoredConnParams ok = true; want false", name)
		}
	}
}

func TestParseAdapterNames(t *testing.T) {
	got := parseAdapterNames([]string{"hci1", "hci0", "hci0:64", "rfkill0", "hcix"})
	if want := []int{0, 1}; !slices.Equal(got, want) {
		t.Errorf("parseAdapterNames = %v; want %v", got, want)
	}
}

type fakeLinkReporter map[string]LinkInfo

func (f fakeLinkReporter) LinkInfo(address string) (LinkInfo, bool) {
	li, ok := f[address]
	return li, ok
}

func TestAnnotateLink(t *testing.T) {
	links := fakeLinkReporter{padAddr: {SupervisionTimeoutMS: 500, RequestedSupervisionTimeoutMS: 3000}}

	p := &agentpb.DiscoveredBluetoothPeripheral{Address: padAddr, Connected: true}
	annotateLink(p, links)
	if p.GetSupervisionTimeoutMs() != 500 || p.GetRequestedSupervisionTimeoutMs() != 3000 {
		t.Errorf("connected pad = %d/%d; want 500/3000", p.GetSupervisionTimeoutMs(), p.GetRequestedSupervisionTimeoutMs())
	}

	idle := &agentpb.DiscoveredBluetoothPeripheral{Address: padAddr}
	annotateLink(idle, links)
	if idle.SupervisionTimeoutMs != nil {
		t.Error("a disconnected peripheral must not carry a link timeout")
	}

	unrequested := &agentpb.DiscoveredBluetoothPeripheral{Address: "11:22:33:44:55:66", Connected: true}
	annotateLink(unrequested, fakeLinkReporter{"11:22:33:44:55:66": {SupervisionTimeoutMS: 420}})
	if unrequested.GetSupervisionTimeoutMs() != 420 || unrequested.RequestedSupervisionTimeoutMs != nil {
		t.Errorf("unrequested = %v/%v; want 420/absent", unrequested.SupervisionTimeoutMs, unrequested.RequestedSupervisionTimeoutMs)
	}

	noReporter := &agentpb.DiscoveredBluetoothPeripheral{Address: padAddr, Connected: true}
	annotateLink(noReporter, nil)
	if noReporter.SupervisionTimeoutMs != nil {
		t.Error("without a reporter nothing is annotated")
	}
}

// ---- watcher harness -------------------------------------------------------

const padAddr = "AA:BB:CC:DD:EE:FF"

// padConn is the Xbox pad as the kernel lists it: an LE link we are central on.
var padConn = connInfo{Handle: 0x0010, Address: padAddr, LinkType: hciLinkLE, Central: true}

// xboxParams is what the Xbox Wireless Controller asks for: 7.5 ms interval,
// latency 0, 3.0 s supervision timeout.
var xboxParams = connParams{Interval: 6, Latency: 0, Timeout: 300}

// ourParams is xboxParams after the watcher's override.
var ourParams = connParams{Interval: 6, Latency: 0, Timeout: 50}

// ourUpdate is the command the watcher sends for the pad.
var ourUpdate = sentUpdate{Handle: 0x0010, Update: connUpdate{IntervalMin: 6, IntervalMax: 6, Latency: 0, Timeout: 50}}

type sentUpdate struct {
	Handle uint16
	Update connUpdate
}

type fakeLinkTransport struct {
	mu     sync.Mutex
	events chan hciEvent
	conns  []connInfo
	stored map[string]connUpdate
	sent   []sentUpdate
}

func newFakeLinkTransport() *fakeLinkTransport {
	return &fakeLinkTransport{events: make(chan hciEvent, 16), stored: map[string]connUpdate{}}
}

func (f *fakeLinkTransport) Events() <-chan hciEvent { return f.events }

func (f *fakeLinkTransport) UpdateConnection(handle uint16, u connUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentUpdate{Handle: handle, Update: u})
	return nil
}

func (f *fakeLinkTransport) Connections() ([]connInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.conns), nil
}

func (f *fakeLinkTransport) StoredParams(address string) (connUpdate, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.stored[address]
	return u, ok
}

func (f *fakeLinkTransport) Close() error { return nil }

func (f *fakeLinkTransport) sentUpdates() []sentUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

type fakeClass struct{ hid, known bool }

type fakeClassifier struct {
	mu sync.Mutex
	m  map[string]fakeClass
}

func (f *fakeClassifier) set(address string, c fakeClass) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[address] = c
}

func (f *fakeClassifier) IsHID(_ context.Context, _ int, address string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.m[address]
	return c.hid, c.known
}

type linkHarness struct {
	t      *testing.T
	tr     *fakeLinkTransport
	cls    *fakeClassifier
	w      *Watcher
	logs   *observer.ObservedLogs
	cancel context.CancelFunc
}

// newLinkHarness builds a watcher over one fake adapter with the pad listed
// and classified as HID. Adjust h.tr and h.cls, then call start.
func newLinkHarness(t *testing.T, target uint16) *linkHarness {
	core, logs := observer.New(zapcore.DebugLevel)
	h := &linkHarness{
		t:    t,
		tr:   newFakeLinkTransport(),
		cls:  &fakeClassifier{m: map[string]fakeClass{padAddr: {hid: true, known: true}}},
		logs: logs,
	}
	h.tr.conns = []connInfo{padConn}
	h.w = newWatcher(zap.New(core), target, linkPlatform{
		adapters: func() ([]int, error) { return []int{0}, nil },
		open:     func(int) (linkTransport, error) { return h.tr, nil },
		classify: h.cls,
	})
	return h
}

func (h *linkHarness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.w.Run(ctx)
	synctest.Wait()
}

func (h *linkHarness) stop() {
	h.cancel()
	synctest.Wait()
}

func (h *linkHarness) emit(ev hciEvent) {
	if ev.Kind == hciDisconnComplete {
		// The kernel finishes handling a disconnect, deleting the
		// connection, before the watcher reads the event and could look it
		// up, so the fake kernel forgets it first.
		h.tr.mu.Lock()
		h.tr.conns = slices.DeleteFunc(h.tr.conns, func(c connInfo) bool { return c.Handle == ev.Handle })
		h.tr.mu.Unlock()
	}
	h.tr.events <- ev
	synctest.Wait()
}

// list makes the fake kernel list a connection, as it does for a new one.
func (h *linkHarness) list(ci connInfo) {
	h.tr.mu.Lock()
	defer h.tr.mu.Unlock()
	h.tr.conns = append(h.tr.conns, ci)
}

func (h *linkHarness) wait(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func (h *linkHarness) connect(p connParams) {
	h.emit(hciEvent{Kind: hciConnComplete, Handle: padConn.Handle, Central: true, Params: p})
}

func (h *linkHarness) updated(p connParams) {
	h.emit(hciEvent{Kind: hciConnUpdateComplete, Handle: padConn.Handle, Params: p})
}

func (h *linkHarness) wantSent(want ...sentUpdate) {
	h.t.Helper()
	if got := h.tr.sentUpdates(); !slices.Equal(got, want) {
		h.t.Fatalf("sent updates = %+v; want %+v", got, want)
	}
}

func (h *linkHarness) wantLink(want LinkInfo, wantOK bool) {
	h.t.Helper()
	got, ok := h.w.LinkInfo(padAddr)
	if ok != wantOK || got != want {
		h.t.Fatalf("LinkInfo = %+v, %v; want %+v, %v", got, ok, want, wantOK)
	}
}

// logged returns the entries with message msg.
func (h *linkHarness) logged(msg string) []observer.LoggedEntry {
	return h.logs.FilterMessage(msg).All()
}

// ---- watcher behavior ------------------------------------------------------

func TestLinkWatch_OverridesPadAfterSettle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.start()
		defer h.stop()

		h.connect(xboxParams)
		h.wantSent() // nothing before the settle delay
		h.wantLink(LinkInfo{SupervisionTimeoutMS: 3000}, true)

		h.wait(linkSettleDelay)
		h.wantSent(ourUpdate)

		h.updated(ourParams)
		h.wantLink(LinkInfo{SupervisionTimeoutMS: 500, RequestedSupervisionTimeoutMS: 3000}, true)
		if n := len(h.logged("Bluetooth link timeout set")); n != 1 {
			t.Errorf("logged %d 'link timeout set' entries; want 1", n)
		}
	})
}

func TestLinkWatch_PadRequestDuringSettleIsOverriddenOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.start()
		defer h.stop()

		// Linux's default: 30–50 ms interval, 420 ms timeout. Then the pad
		// asks for 3 s before the settle delay is up.
		h.connect(connParams{Interval: 40, Latency: 0, Timeout: 42})
		h.updated(xboxParams)
		h.wantSent(ourUpdate)
		h.updated(ourParams)

		h.wait(linkSettleDelay)
		h.wantSent(ourUpdate) // the settle check found the link already at 500 ms
	})
}

func TestLinkWatch_GuardGivesUpAfterThreeRaises(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.start()
		defer h.stop()

		h.connect(ourParams)
		for range 2 {
			h.updated(xboxParams) // the pad drags it back up
			h.updated(ourParams)  // and we put it back
			h.wait(time.Second)
		}
		h.wantSent(ourUpdate, ourUpdate)

		h.updated(xboxParams) // third raise within 60 s
		h.wantSent(ourUpdate, ourUpdate)
		h.wantLink(LinkInfo{SupervisionTimeoutMS: 3000, RequestedSupervisionTimeoutMS: 3000}, true)
		if n := len(h.logged("Bluetooth device keeps restoring its own link timeout; leaving it")); n != 1 {
			t.Errorf("logged %d guard warnings; want 1", n)
		}

		h.updated(xboxParams)
		h.wantSent(ourUpdate, ourUpdate) // stays given up for this connection
	})
}

func TestLinkWatch_GuardForgetsRaisesOlderThanItsWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.start()
		defer h.stop()

		h.connect(ourParams)
		for range 3 {
			h.updated(xboxParams)
			h.updated(ourParams)
			h.wait(linkGuardWindow)
		}
		h.wantSent(ourUpdate, ourUpdate, ourUpdate)
	})
}

func TestLinkWatch_LeavesLinksItShouldNotTouch(t *testing.T) {
	tests := []struct {
		name    string
		target  uint16
		central bool
		class   fakeClass
	}{
		{"override off", 0, true, fakeClass{hid: true, known: true}},
		{"we are peripheral", 50, false, fakeClass{hid: true, known: true}},
		{"not a HID device", 50, true, fakeClass{hid: false, known: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newLinkHarness(t, tt.target)
				h.cls.set(padAddr, tt.class)
				h.tr.conns[0].Central = tt.central
				h.start()
				defer h.stop()

				h.emit(hciEvent{Kind: hciConnComplete, Handle: padConn.Handle, Central: tt.central, Params: xboxParams})
				h.wait(linkSettleDelay)
				h.updated(xboxParams)
				h.wantSent()
				h.wantLink(LinkInfo{SupervisionTimeoutMS: 3000}, true) // still reported
			})
		})
	}
}

func TestLinkWatch_ClassifiesHIDOnceBlueZKnows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.cls.set(padAddr, fakeClass{}) // services not resolved yet
		h.start()
		defer h.stop()

		h.connect(xboxParams)
		h.wait(linkSettleDelay)
		h.wantSent()

		h.cls.set(padAddr, fakeClass{hid: true, known: true})
		h.updated(xboxParams) // the pad's request after service discovery
		h.wantSent(ourUpdate)
	})
}

func TestLinkWatch_SettlesAtTheFloorWhenItExceedsTheTarget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.start()
		defer h.stop()

		slow := connParams{Interval: 80, Latency: 4, Timeout: 300} // floor 1010 ms
		h.connect(slow)
		h.wait(linkSettleDelay)
		floor := sentUpdate{Handle: 0x0010, Update: connUpdate{IntervalMin: 80, IntervalMax: 80, Latency: 4, Timeout: 101}}
		h.wantSent(floor)

		h.updated(connParams{Interval: 80, Latency: 4, Timeout: 101})
		h.updated(connParams{Interval: 80, Latency: 4, Timeout: 101})
		h.wantSent(floor)
	})
}

func TestLinkWatch_RetriesBusyControllerThenStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.start()
		defer h.stop()

		h.connect(xboxParams)
		h.wait(linkSettleDelay)
		busy := hciEvent{Kind: hciCmdStatus, Status: hciStatusControllerBusy, Opcode: hciOpLEConnUpdate}
		for range linkMaxRetries {
			h.emit(busy)
			h.wait(linkRetryDelay)
		}
		h.wantSent(ourUpdate, ourUpdate, ourUpdate, ourUpdate) // first try + 3 retries

		h.emit(busy)
		h.wait(linkRetryDelay)
		h.wantSent(ourUpdate, ourUpdate, ourUpdate, ourUpdate)
		if n := len(h.logged("Gave up setting the Bluetooth link timeout; the controller stayed busy")); n != 1 {
			t.Errorf("logged %d give-up warnings; want 1", n)
		}
	})
}

func TestLinkWatch_IgnoresFailedStatusWithNothingInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.start()
		defer h.stop()

		h.connect(ourParams)
		h.emit(hciEvent{Kind: hciCmdStatus, Status: hciStatusControllerBusy, Opcode: hciOpLEConnUpdate})
		h.wait(linkRetryDelay)
		h.wantSent()
		if n := h.logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
			t.Errorf("logged %d warnings; want none", n)
		}
	})
}

func TestLinkWatch_DeviceRefusalWarnsOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.start()
		defer h.stop()

		h.connect(xboxParams)
		h.wait(linkSettleDelay)
		refused := hciEvent{Kind: hciConnUpdateComplete, Status: 0x3B, Handle: padConn.Handle, Params: xboxParams}
		h.emit(refused)
		h.updated(xboxParams) // a later raise re-arms the override
		h.emit(refused)
		h.wantSent(ourUpdate, ourUpdate)
		if n := len(h.logged("Bluetooth device refused the link timeout update")); n != 1 {
			t.Errorf("logged %d refusal warnings; want 1", n)
		}
	})
}

func TestLinkWatch_InFlightUpdateExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.start()
		defer h.stop()

		h.connect(xboxParams)
		h.wait(linkSettleDelay)
		h.wantSent(ourUpdate) // its completion never arrives

		h.updated(xboxParams)
		h.wantSent(ourUpdate) // still in flight: nothing new

		h.wait(linkInFlightTimeout)
		h.updated(xboxParams)
		h.wantSent(ourUpdate, ourUpdate)
	})
}

func TestLinkWatch_LogsDisconnectReasons(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const speaker = "11:22:33:44:55:66"
		h := newLinkHarness(t, 50)
		h.tr.conns = append(h.tr.conns, connInfo{Handle: 0x000B, Address: speaker, LinkType: hciLinkACL, Central: true})
		h.cls.set(speaker, fakeClass{hid: false, known: true})
		h.start()
		defer h.stop()

		h.connect(xboxParams)
		h.wait(linkSettleDelay)
		h.updated(ourParams)
		h.wait(10 * time.Second)
		h.emit(hciEvent{Kind: hciDisconnComplete, Handle: padConn.Handle, Reason: 0x08})

		lost := h.logged("Bluetooth HID link lost")
		if len(lost) != 1 || lost[0].Level != zapcore.WarnLevel {
			t.Fatalf("HID link lost entries = %+v; want one Warn", lost)
		}
		fields := lost[0].ContextMap()
		for k, want := range map[string]any{
			"address":                padAddr,
			"adapter":                "hci0",
			"link_type":              "le",
			"reason_code":            "0x08",
			"reason":                 "connection timeout",
			"supervision_timeout_ms": uint32(500),
			"duration":               11 * time.Second,
		} {
			if fields[k] != want {
				t.Errorf("field %s = %v (%T); want %v (%T)", k, fields[k], fields[k], want, want)
			}
		}
		h.wantLink(LinkInfo{}, false)

		// A Classic speaker the watch never saw connect, turned off at the speaker.
		h.emit(hciEvent{Kind: hciDisconnComplete, Handle: 0x000B, Reason: 0x13})
		closed := h.logged("Bluetooth link closed")
		if len(closed) != 1 || closed[0].Level != zapcore.InfoLevel {
			t.Fatalf("link closed entries = %+v; want one Info", closed)
		}
		fields = closed[0].ContextMap()
		if fields["address"] != speaker || fields["link_type"] != "classic" || fields["reason"] != "remote user terminated connection" {
			t.Errorf("speaker disconnect fields = %v", fields)
		}
		if _, ok := fields["supervision_timeout_ms"]; ok {
			t.Error("a Classic link must not report an LE supervision timeout")
		}
	})
}

func TestLinkWatch_AdoptsExistingLinkFromStoredParams(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.tr.stored[padAddr] = connUpdate{IntervalMin: 6, IntervalMax: 6, Latency: 0, Timeout: 300}
		h.start()
		defer h.stop()

		h.wantSent(ourUpdate) // on start, with no connect event
		h.wantLink(LinkInfo{}, false)

		h.updated(ourParams)
		h.wantLink(LinkInfo{SupervisionTimeoutMS: 500, RequestedSupervisionTimeoutMS: 3000}, true)
	})
}

func TestLinkWatch_AdoptWithoutStoredParamsWaitsForReconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.start()
		defer h.stop()

		h.wantSent()
		if n := len(h.logged("No stored parameters for a connected Bluetooth HID device; its link timeout is set when it next connects")); n != 1 {
			t.Errorf("logged %d no-stored-parameters entries; want 1", n)
		}
	})
}

func TestLinkWatch_OneUpdateInFlightPerAdapter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pad2 = "AA:BB:CC:DD:EE:00"
		h := newLinkHarness(t, 50)
		h.tr.conns = append(h.tr.conns, connInfo{Handle: 0x0011, Address: pad2, LinkType: hciLinkLE, Central: true})
		h.cls.set(pad2, fakeClass{hid: true, known: true})
		h.start()
		defer h.stop()

		h.connect(xboxParams)
		h.wait(100 * time.Millisecond) // so the two settle checks fire in a known order
		h.emit(hciEvent{Kind: hciConnComplete, Handle: 0x0011, Central: true, Params: xboxParams})
		h.wait(linkSettleDelay)
		h.wantSent(ourUpdate) // the second pad waits its turn

		h.updated(ourParams)
		h.wait(linkRetryDelay)
		h.wantSent(ourUpdate, sentUpdate{Handle: 0x0011, Update: ourUpdate.Update})
	})
}

func TestWatcherRun_RetriesAnAdapterThatFailsToOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		tr := newFakeLinkTransport()
		var opens atomic.Int32
		w := newWatcher(zap.New(core), 50, linkPlatform{
			adapters: func() ([]int, error) { return []int{0}, nil },
			open: func(int) (linkTransport, error) {
				if opens.Add(1) < 3 {
					return nil, errors.New("permission denied")
				}
				return tr, nil
			},
			classify: &fakeClassifier{m: map[string]fakeClass{}},
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer func() { cancel(); synctest.Wait() }()
		go w.Run(ctx)
		synctest.Wait()

		time.Sleep(1 * time.Second) // first backoff
		synctest.Wait()
		time.Sleep(2 * time.Second) // second backoff
		synctest.Wait()
		if n := opens.Load(); n != 3 {
			t.Errorf("open attempts = %d; want 3", n)
		}
		if n := logs.FilterMessage("Cannot watch Bluetooth adapter links").Len(); n != 1 {
			t.Errorf("logged %d open failures; want 1 (repeats of the same error are quiet)", n)
		}
	})
}

func TestWatcherRun_ReopensAnAdapterThatGoesAway(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := newFakeLinkTransport(), newFakeLinkTransport()
		var opens atomic.Int32
		w := newWatcher(zap.NewNop(), 50, linkPlatform{
			adapters: func() ([]int, error) { return []int{0}, nil },
			open: func(int) (linkTransport, error) {
				if opens.Add(1) == 1 {
					return first, nil
				}
				return second, nil
			},
			classify: &fakeClassifier{m: map[string]fakeClass{}},
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer func() { cancel(); synctest.Wait() }()
		go w.Run(ctx)
		synctest.Wait()

		close(first.events) // the adapter was unplugged
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		if n := opens.Load(); n != 2 {
			t.Errorf("open attempts = %d; want 2", n)
		}
	})
}

// ---- review focus ----------------------------------------------------------

func TestLinkWatch_ResolvesAddressLate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.tr.conns = nil // the kernel has not listed the new link yet
		h.start()
		defer h.stop()

		go func() {
			time.Sleep(600 * time.Millisecond) // past linkAddressWait
			h.tr.mu.Lock()
			h.tr.conns = []connInfo{padConn}
			h.tr.mu.Unlock()
		}()
		h.connect(xboxParams)
		h.wantLink(LinkInfo{}, false) // no address yet, so nothing to report

		h.wait(linkSettleDelay + linkAddressWait)
		h.wantSent(ourUpdate)
	})
}

func TestLinkWatch_ReconnectStartsFresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.start()
		defer h.stop()

		h.connect(ourParams)
		for range linkGuardMaxRaises {
			h.updated(xboxParams)
			h.updated(ourParams)
		}
		h.wantSent(ourUpdate, ourUpdate) // the third raise tripped the guard

		h.emit(hciEvent{Kind: hciDisconnComplete, Handle: padConn.Handle, Reason: 0x13})
		h.list(padConn)
		h.connect(xboxParams) // same handle, new connection
		h.wait(linkSettleDelay)
		h.wantSent(ourUpdate, ourUpdate, ourUpdate)
	})
}

func TestLinkWatch_AdoptKeepsTheStoredIntervalRange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.tr.stored[padAddr] = connUpdate{IntervalMin: 6, IntervalMax: 9, Latency: 0, Timeout: 300}
		h.start()
		defer h.stop()

		h.wantSent(sentUpdate{Handle: 0x0010, Update: connUpdate{IntervalMin: 6, IntervalMax: 9, Latency: 0, Timeout: 50}})
	})
}

func TestLinkWatch_LogsClassicLinkThatConnectsAfterStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const dualSense = "11:22:33:44:55:66"
		h := newLinkHarness(t, 50)
		h.cls.set(dualSense, fakeClass{hid: true, known: true})
		h.start()
		defer h.stop()

		h.list(connInfo{Handle: 0x000B, Address: dualSense, LinkType: hciLinkACL, Central: true})
		h.emit(hciEvent{Kind: hciClassicConnComplete, Handle: 0x000B, Address: dualSense, LinkType: hciLinkACL})
		h.wait(linkSettleDelay)
		h.wantSent() // Classic links are only logged
		h.wait(4 * time.Second)
		h.emit(hciEvent{Kind: hciDisconnComplete, Handle: 0x000B, Reason: 0x08})

		lost := h.logged("Bluetooth HID link lost")
		if len(lost) != 1 {
			t.Fatalf("HID link lost entries = %d; want 1", len(lost))
		}
		f := lost[0].ContextMap()
		if f["address"] != dualSense || f["link_type"] != "classic" || f["duration"] != 5*time.Second {
			t.Errorf("Classic disconnect fields = %v", f)
		}
	})
}

func TestLinkWatch_DisconnectOfAnUnknownHandleIsLogged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.start()
		defer h.stop()

		h.emit(hciEvent{Kind: hciDisconnComplete, Handle: 0x0042, Reason: 0x08})
		closed := h.logged("Bluetooth link closed")
		if len(closed) != 1 {
			t.Fatalf("link closed entries = %d; want 1", len(closed))
		}
		if f := closed[0].ContextMap(); f["link_type"] != "unknown" || f["address"] != "" {
			t.Errorf("unknown-handle fields = %v; want link_type unknown and an empty address", f)
		}
	})
}

func TestWatcherRun_ForgetsLinksOfAnAdapterThatGoesAway(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newLinkHarness(t, 50)
		h.start()
		defer h.stop()

		h.connect(xboxParams)
		h.wantLink(LinkInfo{SupervisionTimeoutMS: 3000}, true)

		close(h.tr.events) // the adapter was unplugged
		synctest.Wait()
		h.wantLink(LinkInfo{}, false)
	})
}

func TestNewWatcher_ReadsTheTargetFromTheEnvironment(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	t.Setenv(hidSupervisionTimeoutEnv, "750")
	if w := NewWatcher(zap.New(core)); w.target != 75 {
		t.Errorf("target = %d; want 75", w.target)
	}
	t.Setenv(hidSupervisionTimeoutEnv, "fast")
	if w := NewWatcher(zap.New(core)); w.target != defaultHIDSupervisionTimeout {
		t.Errorf("target = %d; want the default", w.target)
	}
	if n := logs.FilterMessage("Invalid Bluetooth HID supervision timeout; using the default").Len(); n != 1 {
		t.Errorf("logged %d invalid-value warnings; want 1", n)
	}
}
