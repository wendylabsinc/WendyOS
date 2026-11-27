package localmesh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

type haveBatchFixture struct {
	now                 time.Time
	credentials         []*Credentials
	records             []SignedManifest
	source, destination *Directory
	sender, receiver    *Synchronizer
}

func newHaveBatchFixture(t testing.TB, count int) *haveBatchFixture {
	t.Helper()
	f := &haveBatchFixture{now: time.Now()}
	for i := 0; i < count; i++ {
		f.credentials = append(f.credentials, testCredentialsFor(t, testPrincipal(testTenantUUID, fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1))))
	}
	for _, target := range []**Directory{&f.source, &f.destination} {
		cache, err := OpenIdentityCache("", DefaultCacheLimits(), f.credentials[0].Verify, f.now)
		if err != nil {
			t.Fatal(err)
		}
		*target, err = NewDirectory(f.credentials[0].Device.Tenant(), count+1, cache)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range f.credentials {
		if _, err := f.source.cache.Put(c.Certificate.Certificate, f.now); err != nil {
			t.Fatal(err)
		}
		w, err := SignManifest(routeManifest(c.Device, f.now), c.Certificate.Certificate, c.Signer, f.now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.source.Accept(w, f.now); err != nil {
			t.Fatal(err)
		}
		f.records = append(f.records, w)
	}
	f.sender = NewSynchronizer(f.source, f.source.cache, netgossip.NewPool())
	f.receiver = NewSynchronizer(f.destination, f.destination.cache, netgossip.NewPool())
	f.sender.haveBatches = true
	f.receiver.haveBatches = true
	t.Cleanup(f.sender.Close)
	t.Cleanup(f.receiver.Close)
	return f
}

func drainHaveBatchTransfers(t *testing.T, first, second *Synchronizer, now time.Time) {
	t.Helper()
	for step := 0; step < 1000; step++ {
		count := 0
		for _, ends := range [][2]*Synchronizer{{first, second}, {second, first}} {
			messages, _, err := ends[0].ContinueCertificates(now, 32)
			if err != nil {
				t.Fatal(err)
			}
			count += len(messages)
			exchangeDirectoryMessages(t, ends[0], ends[1], messages, now)
		}
		if count == 0 {
			return
		}
	}
	t.Fatal("batch transfers did not quiesce")
}

func TestManifestHaveBatchColdAndWarmFraming(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			f := newHaveBatchFixture(t, 33)
			f.sender.haveBatches = enabled
			f.receiver.haveBatches = enabled
			f.sender.seedAnnouncements(f.credentials[0].Device.Key(), f.now)
			initial := f.sender.drainAnnouncements(f.now, 128)
			want := 33
			if enabled {
				want = 1 + (32+manifestHavesPerFrame-1)/manifestHavesPerFrame
			}
			if len(initial) != want || initial[0].Gossip == nil || initial[0].Gossip.ID() != manifestGossipID(f.records[0]) {
				t.Fatalf("inventory count/order: %d", len(initial))
			}
			frames, size := 0, 0
			for _, message := range initial {
				var wire bytes.Buffer
				if err := WriteControl(&wire, message); err != nil {
					t.Fatal(err)
				}
				size += wire.Len()
				frames++
				decoded, err := ReadControl(&wire)
				if err != nil {
					t.Fatal(err)
				}
				if decoded.Kind == "manifest-haves" && (len(decoded.Haves) > manifestHavesPerFrame*manifestDigestSize || len(decoded.haveEntries) != 0) {
					t.Fatal("bad wire ownership/bound")
				}
				exchangeDirectoryMessages(t, f.sender, f.receiver, []ControlMessage{decoded}, f.now)
				drainHaveBatchTransfers(t, f.sender, f.receiver, f.now)
			}
			if len(f.destination.Snapshot(f.now)) != 33 {
				t.Fatal("cold inventory failed to converge")
			}
			// A new carrier preserves the pool, but still sends the whole inventory.
			f.sender.Close()
			f.receiver.Close()
			f.sender = NewSynchronizer(f.source, f.source.cache, f.sender.gossipPool)
			f.receiver = NewSynchronizer(f.destination, f.destination.cache, f.receiver.gossipPool)
			defer f.sender.Close()
			defer f.receiver.Close()
			f.sender.haveBatches = enabled
			f.receiver.haveBatches = enabled
			f.sender.seedAnnouncements(f.credentials[0].Device.Key(), f.now)
			for _, message := range f.sender.drainAnnouncements(f.now, 128) {
				out, changed, err := f.receiver.Receive(message, f.now, 32)
				if err != nil || len(out) != 0 || len(changed) != 0 {
					t.Fatalf("warm inventory transferred data: %v %d %d", err, len(out), len(changed))
				}
			}
			t.Logf("33-origin inventory: enabled=%t frames=%d framed_bytes=%d", enabled, frames, size)
		})
	}
}

func TestManifestHaveBatchValidationIsAtomic(t *testing.T) {
	f := newHaveBatchFixture(t, 3)
	f.sender.seedAnnouncements("", f.now)
	valid := packHaveBatch(f.sender.drainSingleAnnouncements(f.now, maxManifestHaves))
	for _, mutate := range []func(*ControlMessage){
		func(m *ControlMessage) { m.Haves = m.Haves[:33] },
		func(m *ControlMessage) { m.Haves = append(m.Haves, m.Haves[:32]...) },
		func(m *ControlMessage) { m.Haves = make([]byte, 17*32) },
		func(m *ControlMessage) { m.Principal = "unexpected" },
	} {
		bad := valid
		bad.Haves = bytes.Clone(valid.Haves)
		mutate(&bad)
		if _, _, err := f.receiver.Receive(bad, f.now, 32); err == nil {
			t.Fatal("accepted invalid batch")
		}
		var wire bytes.Buffer
		if err := WriteControl(&wire, bad); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadControl(&wire); err == nil {
			t.Fatal("decoder accepted invalid batch")
		}
	}
	f.receiver.haveBatches = false
	if _, _, err := f.receiver.Receive(valid, f.now, 32); err == nil {
		t.Fatal("unnegotiated batch accepted")
	}
	f.receiver.haveBatches = true
	out, _, err := f.receiver.Receive(valid, f.now, 32)
	if err != nil || len(out) != 3 {
		t.Fatalf("invalid batch consumed hints: %d %v", len(out), err)
	}
}

func TestManifestHaveBatchReplyReserveAndRetry(t *testing.T) {
	f := newHaveBatchFixture(t, maxManifestHaves)
	f.sender.seedAnnouncements("", f.now)
	batch := packHaveBatch(f.sender.drainSingleAnnouncements(f.now, maxManifestHaves))
	// Every attach path uses the same nodePeer reserve, independently of the
	// writer channel size. Even the deliberately constrained zero-slot fixture
	// admits the largest batch when its retained reserve is empty.
	for _, capacity := range []int{0, 1, 128} {
		p := &nodePeer{control: make(chan ControlMessage, capacity)}
		if directoryResponseSlots(p) < maxManifestHaves {
			t.Fatalf("capacity %d cannot admit max batch", capacity)
		}
	}
	n := &Node{directory: f.destination}
	p := &nodePeer{sync: f.receiver, haveBatches: true, control: make(chan ControlMessage, 1)}
	filler := ControlMessage{Kind: "identity-request", Principal: f.credentials[0].Device.Principal()}
	for range maxDirectoryReplyFrames + 1 {
		if !n.queueReplies(p, []ControlMessage{filler}) {
			t.Fatal("reserve fill failed")
		}
	}
	before := f.receiver.gossipPool.PayloadStats()
	if out, changed, err := f.receiver.Receive(batch, f.now, directoryResponseSlots(p)); !errors.Is(err, ErrControlBackpressure) || len(out) != 0 || len(changed) != 0 {
		t.Fatal("batch failed atomic preflight", err)
	}
	if after := f.receiver.gossipPool.PayloadStats(); after != before {
		t.Fatal("blocked batch changed pool", before, after)
	}
	drained := 0
	for directoryResponseSlots(p) < maxManifestHaves {
		<-p.control
		drained++
		n.flushReplies(p)
	}
	out, _, err := f.receiver.Receive(batch, f.now, directoryResponseSlots(p))
	if err != nil || len(out) == 0 || !n.queueReplies(p, out) {
		t.Fatal("batch did not resume", err)
	}
	// All remaining hints stay in the gossip scheduler and complete when the
	// four concurrent WANTs finish; no prefix is replayed and no WANT is lost.
	var requests []ControlMessage
	for len(p.control) > 0 || len(p.replies) > 0 {
		m := <-p.control
		n.flushReplies(p)
		if m.Gossip != nil {
			requests = append(requests, m)
		}
	}
	exchangeDirectoryMessages(t, f.receiver, f.sender, requests, f.now)
	drainHaveBatchTransfers(t, f.sender, f.receiver, f.now)
	if len(f.destination.Snapshot(f.now)) != maxManifestHaves {
		t.Fatal("pressured retry lost inventory entries")
	}
	t.Logf("maximum batch resumed after draining %d prior replies", drained)
}

type haveBatchCapture struct{ messages []ControlMessage }

func (*haveBatchCapture) Close() error                                    { return nil }
func (*haveBatchCapture) SendDatagram([]byte) error                       { return nil }
func (*haveBatchCapture) ReceiveDatagram(context.Context) ([]byte, error) { return nil, io.EOF }
func (l *haveBatchCapture) WriteControl(m ControlMessage) error {
	l.messages = append(l.messages, m)
	return nil
}
func (*haveBatchCapture) ReadControl() (ControlMessage, error) { return ControlMessage{}, io.EOF }

func TestManifestHaveBatchPerOriginExportChecks(t *testing.T) {
	for _, stage := range []string{"queue", "sweep", "write"} {
		for _, reason := range []string{"identity", "expiry", "supersession"} {
			t.Run(stage+"/"+reason, func(t *testing.T) {
				f := newHaveBatchFixture(t, 3)
				f.sender.seedAnnouncements("", f.now)
				batch := packHaveBatch(f.sender.drainSingleAnnouncements(f.now, maxManifestHaves))
				link := &haveBatchCapture{}
				p := &nodePeer{sync: f.sender, haveBatches: true, control: make(chan ControlMessage, 1), link: link}
				n := &Node{directory: f.source, peers: map[babel.LinkID]*nodePeer{1: p}}
				if stage == "sweep" {
					n.queue(p, []ControlMessage{batch})
				}
				stale := batch.haveEntries[0]
				switch reason {
				case "identity":
					f.source.cache.mu.Lock()
					delete(f.source.cache.entries, stale.origin)
					f.source.cache.mu.Unlock()
				case "expiry":
					batch.haveEntries[0].deadline = f.now.Add(-time.Second)
				case "supersession":
					stale.canceled.Store(true)
				}
				switch stage {
				case "queue":
					n.queue(p, []ControlMessage{batch})
				case "sweep":
					n.sweepAnnouncements(f.now)
				case "write":
					if err := n.writePeerControl(p, batch); err != nil {
						t.Fatal(err)
					}
				}
				var got ControlMessage
				if stage == "write" {
					if len(link.messages) != 1 {
						t.Fatal("healthy siblings lost")
					}
					got = link.messages[0]
				} else {
					if len(p.control) != 1 {
						t.Fatal("healthy siblings lost")
					}
					got = <-p.control
				}
				if got.Kind != "manifest-haves" || len(got.Haves) != 64 || bytes.Contains(got.Haves, stale.Gossip.Digest) {
					t.Fatal("stale origin survived final export")
				}
				if reason == "identity" {
					if _, err := f.source.cache.Put(f.credentials[0].Certificate.Certificate, f.now); err != nil {
						t.Fatal(err)
					}
					n.sweepAnnouncements(f.now)
					n.flushAnnouncements(p, f.now)
					if len(p.control) != 1 || (<-p.control).origin != stale.origin {
						t.Fatal("identity repair lost skipped origin")
					}
				}
			})
		}
	}
}

// CBOR transport decoding ignores additive hello fields on older peers. It
// still consumes unknown control kinds rather than acknowledging support.
func TestManifestHaveBatchLegacyUnknownKind(t *testing.T) {
	type oldHeader struct {
		Kind string `cbor:"k"`
	}
	data, err := netwire.Marshal(ControlMessage{Kind: "manifest-haves", Haves: make([]byte, 64)})
	if err != nil {
		t.Fatal(err)
	}
	var old oldHeader
	if err = netwire.Unmarshal(data, &old); err != nil || old.Kind != "manifest-haves" {
		t.Fatal(err)
	}
}

func TestManifestHaveBatchKnownReceiptsRepairAllIdentities(t *testing.T) {
	f := newHaveBatchFixture(t, maxManifestHaves)
	f.sender.seedAnnouncements("", f.now)
	batch := packHaveBatch(f.sender.drainSingleAnnouncements(f.now, maxManifestHaves))
	exchangeDirectoryMessages(t, f.sender, f.receiver, []ControlMessage{batch}, f.now)
	drainHaveBatchTransfers(t, f.sender, f.receiver, f.now)
	if len(f.destination.Snapshot(f.now)) != maxManifestHaves {
		t.Fatal("initial inventory incomplete")
	}
	deadlines := make(map[string]time.Time)
	for _, w := range f.records {
		deadlines[w.ownerKey()], _ = f.destination.RecordDeadline(w, f.now)
	}
	f.destination.cache.mu.Lock()
	clear(f.destination.cache.entries)
	f.destination.cache.mu.Unlock()
	if out, _, err := f.receiver.Receive(batch, f.now, maxManifestHaves-1); !errors.Is(err, ErrControlBackpressure) || len(out) != 0 {
		t.Fatal("partial identity repair consumed batch", err)
	}
	repairs, _, err := f.receiver.Receive(batch, f.now, maxManifestHaves)
	if err != nil || len(repairs) != maxManifestHaves {
		t.Fatal("known receipts suppressed identity repairs", len(repairs), err)
	}
	for _, m := range repairs {
		if m.Kind != "identity-request" {
			t.Fatal("unexpected repair", m.Kind)
		}
	}
	exchangeDirectoryMessages(t, f.receiver, f.sender, repairs, f.now)
	drainHaveBatchTransfers(t, f.sender, f.receiver, f.now)
	for _, w := range f.records {
		deadline, ok := f.destination.RecordDeadline(w, f.now)
		if !ok || deadline != deadlines[w.ownerKey()] {
			t.Fatal("identity repair changed original lease")
		}
	}
}
