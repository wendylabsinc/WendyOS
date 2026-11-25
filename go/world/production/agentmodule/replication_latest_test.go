package agentmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (c moduleReplicaClient) PushLatest(ctx context.Context, q *replicav1.PushLatestRequest, _ ...grpc.CallOption) (*replicav1.PushLatestReply, error) {
	return c.server.PushLatest(context.WithValue(ctx, moduleReplicaPeerKey{}, c.chain), q)
}

func latestQueueValue(entity string, sequence uint64, body []byte) replicaLatestValue {
	v := replicaLatestValue{item: replication.Item{Envelope: body}, sampleFrom: moduleTestNow, physicalFrom: moduleTestNow, physicalUntil: moduleTestNow.Add(time.Minute)}
	v.item.Entry.ExpiresAtUnixNS = moduleTestNow.Add(time.Minute).UnixNano()
	v.item.Entry.Reference.Key = world.Key{Tenant: "tenant", Site: "site", Entity: entity, Type: "capture", Source: world.Source{Device: "device", App: "app", Service: "camera", Stream: "samples"}}
	v.item.Entry.Reference.Version = world.Version{Epoch: 1, Sequence: sequence}
	return v
}

func TestReplicaLatestQueueCoalescesOwnedFullKeysAndNeverRequeues(t *testing.T) {
	q := newReplicaLatestQueue()
	first := latestQueueValue("one", 1, []byte("first"))
	if !q.offer(first) {
		t.Fatal("first offer")
	}
	first.item.Envelope[0] = 'X'
	values, more := q.take()
	if more || len(values) != 1 || string(values[0].item.Envelope) != "first" {
		t.Fatalf("unowned bytes: %+v", values)
	}
	if q.offer(first) {
		t.Fatal("drained version retried")
	}
	newer := latestQueueValue("one", 3, []byte("newest"))
	if !q.offer(newer) || q.offer(latestQueueValue("one", 2, []byte("late"))) {
		t.Fatal("version ordering")
	}
	other := latestQueueValue("one", 1, []byte("other source"))
	other.item.Entry.Reference.Key.Source.Service = "other-camera"
	if !q.offer(other) {
		t.Fatal("source lost from key")
	}
	third := latestQueueValue("one", 1, []byte("other component"))
	third.item.Entry.Reference.Key.Type = "audio"
	if !q.offer(third) {
		t.Fatal("component lost from key")
	}
	values, more = q.take()
	if !more || len(values) != 2 || string(values[0].item.Envelope) != "newest" || string(values[1].item.Envelope) != "other source" {
		t.Fatalf("coalescing/order: %+v", values)
	}
	values, more = q.take()
	if more || len(values) != 1 || string(values[0].item.Envelope) != "other component" || q.bytes != 0 {
		t.Fatal("bounded drain")
	}
	newer.item.Entry.Reference.Version = world.Version{Epoch: 2, Sequence: 1}
	if !q.offer(newer) {
		t.Fatal("new source epoch rejected")
	}
}

func TestReplicaLatestQueueBoundsAndNonblockingWake(t *testing.T) {
	q := newReplicaLatestQueue()
	if q.offer(latestQueueValue("oversized", 1, make([]byte, maxReplicaLatestBytes+1))) {
		t.Fatal("byte bound")
	}
	if q.offer(latestQueueValue("oversized", 1, []byte("retry"))) {
		t.Fatal("dropped version retried")
	}
	for i := 1; i < maxReplicaLatestKeys; i++ {
		if !q.offer(latestQueueValue(fmt.Sprint(i), 1, []byte{1})) {
			t.Fatalf("key %d", i)
		}
	}
	if q.offer(latestQueueValue("excess", 1, []byte{1})) {
		t.Fatal("key bound")
	}
	for len(q.values) != 0 {
		q.take()
	}
	if q.offer(latestQueueValue("excess", 2, []byte{1})) {
		t.Fatal("still-live unknown marks forgotten")
	}
	q.advance(moduleTestNow.Add(time.Minute), moduleTestNow)
	fresh := latestQueueValue("excess", 2, []byte{1})
	fresh.sampleFrom = moduleTestNow.Add(time.Minute)
	fresh.item.Entry.ExpiresAtUnixNS = moduleTestNow.Add(2 * time.Minute).UnixNano()
	if !q.offer(fresh) || len(q.versions) != 1 {
		t.Fatal("expired sample marks were not reclaimed")
	}
	m := &Manager{replica: &replicaState{latestWake: make(chan struct{}, 1)}}
	for i := 0; i < 1000; i++ {
		m.signalReplicaLatest()
	}
	if len(m.replica.latestWake) != 1 {
		t.Fatal("wake is not coalesced")
	}
	if (*Manager)(nil).ReplicaLatestWake() != nil {
		t.Fatal("disabled wake")
	}
}

func TestReplicaLatestQueueTrackChurnAndOriginalExpiry(t *testing.T) {
	q := newReplicaLatestQueue()
	var old replicaLatestValue
	for i := 0; i < 2*maxReplicaLatestKeys; i++ {
		v := latestQueueValue(fmt.Sprint(i), 1, []byte("track"))
		v.physicalFrom = moduleTestNow.Add(time.Duration(i) * time.Second)
		v.physicalUntil = v.physicalFrom.Add(time.Second)
		if !q.offer(v) {
			t.Fatalf("track churn blocked at %d", i)
		}
		q.take() // Unknown delivery still owns the mark until original expiry.
		if q.offer(v) {
			t.Fatal("live unknown requeued")
		}
		if i == 0 {
			old = v
		}
	}
	if len(q.versions) != 1 || q.offer(old) {
		t.Fatal("expired original reintroduced after mark reclamation")
	}
}

func TestReplicaLatestShorterReplacementRetainsUnknownVersionMark(t *testing.T) {
	q := newReplicaLatestQueue()
	old := latestQueueValue("track", 1, []byte("unknown delivery"))
	if !q.offer(old) {
		t.Fatal("first offer")
	}
	q.take()
	newer := latestQueueValue("track", 2, []byte("shorter replacement"))
	newer.item.Entry.ExpiresAtUnixNS = moduleTestNow.Add(time.Second).UnixNano()
	newer.physicalUntil = moduleTestNow.Add(time.Second)
	if !q.offer(newer) {
		t.Fatal("replacement")
	}
	q.take()
	q.advance(moduleTestNow.Add(2*time.Second), moduleTestNow.Add(2*time.Second))
	if q.offer(old) || len(q.versions) != 1 {
		t.Fatal("older live unknown was revived by shorter replacement")
	}
	q.advance(moduleTestNow.Add(time.Minute), moduleTestNow.Add(time.Minute))
	if len(q.versions) != 0 || q.offer(old) {
		t.Fatal("original expired mark did not retire")
	}
}

func latestOffer(t *testing.T, a *moduleReplicaFixture, proof serviceorigin.SignedRecord) []byte {
	t.Helper()
	raw, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	a.m.offerReplicaLatest(proof)
	return raw
}

func latestTake(t *testing.T, a *moduleReplicaFixture) []replication.Item {
	t.Helper()
	items, err := a.m.TakeReplicaLatest(context.Background(), a.peer.cert.Principal)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func latestRecord(t *testing.T, items []replication.Item, raw []byte) replication.Item {
	t.Helper()
	last := ""
	var record replication.Item
	for _, item := range items {
		if item.Entry.OrderKey() <= last {
			t.Fatal("authority order")
		}
		last = item.Entry.OrderKey()
		if item.Entry.Kind == replication.Record {
			if record.Envelope != nil {
				t.Fatal("multiple records")
			}
			record = item
		}
	}
	if !bytes.Equal(record.Envelope, raw) {
		t.Fatal("original envelope changed")
	}
	return record
}

func TestReplicaLatestOriginalFloorLeaseTransferAndDuplicateRetirement(t *testing.T) {
	a, b := newReplicaPair(t)
	startReplicaPair(t, a, b)
	p := a.publisher(t, 0, 1)
	a.publish(t, p, replicaRequest())
	a.m.Release(a.config.Policy.Grants[0].App, "sensor", 1)
	p = a.publisher(t, 0, 2)
	proof := a.publish(t, p, replicaRequest())
	raw := latestOffer(t, a, proof)
	items := latestTake(t, a)
	if len(items) != 3 || items[0].Entry.Kind != replication.Floor || items[1].Entry.Kind != replication.Lease {
		t.Fatalf("missing authority: %+v", items)
	}
	record := latestRecord(t, items, raw)
	if record.Entry.ExpiresAtUnixNS != moduleTestNow.Add(2*time.Second).UnixNano() {
		t.Fatal("sample expiry changed")
	}
	connection, err := a.transport().Dial(context.Background(), b.cert.Principal)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close.Close()
	result, err := a.m.SendReplicaLatest(context.Background(), b.cert.Principal, connection, items)
	if err != nil || !result.Complete || result.Admitted != 3 {
		t.Fatalf("push: %+v %v", result, err)
	}
	live := b.live(t)
	if len(live) != 1 || !bytes.Equal(live[0].Record, proof.Record) {
		t.Fatal("receiver original")
	}
	latestOffer(t, a, proof)
	if len(latestTake(t, a)) != 0 {
		t.Fatal("sent original requeued")
	}
	// The receiver also treats an externally duplicated original as a skip.
	result, err = a.m.SendReplicaLatest(context.Background(), b.cert.Principal, connection, items)
	if err != nil || !result.Complete || result.Skipped != 3 {
		t.Fatalf("receiver duplicate: %+v %v", result, err)
	}
}

func TestReplicaLatestDropsExpiredAndRetiredSources(t *testing.T) {
	for _, mode := range []string{"sample", "physical", "retired", "wrong-export"} {
		t.Run(mode, func(t *testing.T) {
			a, b := newReplicaPair(t)
			startReplicaPair(t, a, b)
			p := a.publisher(t, 0, 1)
			proof := a.publish(t, p, replicaRequest())
			if mode == "wrong-export" {
				a.m.replica.peers[b.cert.Principal].policy.Export = nil
			}
			latestOffer(t, a, proof)
			switch mode {
			case "sample":
				a.sample.Store(moduleTestNow.Add(2 * time.Second).UnixNano())
			case "physical":
				q := a.m.replica.peers[b.cert.Principal].latest
				q.mu.Lock()
				for key, v := range q.values {
					v.physicalUntil = moduleTestNow.Add(time.Millisecond)
					q.values[key] = v
				}
				q.mu.Unlock()
			case "retired":
				a.m.Release(a.config.Policy.Grants[0].App, "sensor", 1)
			}
			if len(latestTake(t, a)) != 0 {
				t.Fatal("invalid live value sent")
			}
			latestOffer(t, a, proof)
			if len(latestTake(t, a)) != 0 {
				t.Fatal("dropped original retried")
			}
		})
	}
}

type latestClient struct {
	replicav1.WorldReplicaServiceClient
	push func(context.Context, *replicav1.PushLatestRequest) (*replicav1.PushLatestReply, error)
}

func (c latestClient) PushLatest(ctx context.Context, q *replicav1.PushLatestRequest, _ ...grpc.CallOption) (*replicav1.PushLatestReply, error) {
	return c.push(ctx, q)
}

func TestReplicaLatestLostReplyDoesNotLatchOrRetryAndFreshValueProgresses(t *testing.T) {
	a, b := newReplicaPair(t)
	startReplicaPair(t, a, b)
	p := a.publisher(t, 0, 1)
	first := a.publish(t, p, replicaRequest())
	latestOffer(t, a, first)
	items := latestTake(t, a)
	connection, err := a.transport().Dial(context.Background(), b.cert.Principal)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close.Close()
	realClient := connection.Client
	var calls atomic.Int32
	connection.Client = latestClient{WorldReplicaServiceClient: realClient, push: func(ctx context.Context, q *replicav1.PushLatestRequest) (*replicav1.PushLatestReply, error) {
		calls.Add(1)
		if _, err := realClient.PushLatest(ctx, q); err != nil {
			return nil, err
		}
		return nil, status.Error(codes.Unavailable, "lost reply after commit")
	}}
	result, err := a.m.SendReplicaLatest(context.Background(), b.cert.Principal, connection, items)
	if !errors.Is(err, replication.ErrUncertain) || !result.UnknownOutcome || a.m.replica.failed.Load() || b.m.replica.failed.Load() {
		t.Fatalf("lost reply: %+v %v", result, err)
	}
	latestOffer(t, a, first)
	if len(latestTake(t, a)) != 0 || calls.Load() != 1 {
		t.Fatal("unknown resent")
	}
	second := a.publish(t, p, replicaRequest())
	latestOffer(t, a, second)
	connection.Client = realClient
	result, err = a.m.SendReplicaLatest(context.Background(), b.cert.Principal, connection, latestTake(t, a))
	if err != nil || !result.Complete {
		t.Fatalf("fresh: %+v %v", result, err)
	}
	if live := b.live(t); len(live) != 1 || !bytes.Equal(live[0].Record, second.Record) {
		t.Fatal("fresh original absent")
	}
}

func TestReplicaLatestSendHoldsNoLocalAdmissionLocks(t *testing.T) {
	a, b := newReplicaPair(t)
	startReplicaPair(t, a, b)
	p := a.publisher(t, 0, 1)
	latestOffer(t, a, a.publish(t, p, replicaRequest()))
	items := latestTake(t, a)
	connection, err := a.transport().Dial(context.Background(), b.cert.Principal)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	realClient := connection.Client
	connection.Client = latestClient{WorldReplicaServiceClient: realClient, push: func(ctx context.Context, q *replicav1.PushLatestRequest) (*replicav1.PushLatestReply, error) {
		close(entered)
		select {
		case <-release:
			return realClient.PushLatest(ctx, q)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.m.SendReplicaLatest(ctx, b.cert.Principal, connection, items); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("send did not enter")
	}
	// Every fence used by ordinary local admission must remain available while
	// the authenticated peer call is blocked.
	if !a.identity.mu.TryLock() {
		t.Fatal("network holds identity fence")
	}
	a.identity.mu.Unlock()
	if !a.m.replica.mu.TryLock() {
		t.Fatal("network holds replica fence")
	}
	a.m.replica.mu.Unlock()
	if !a.m.mu.TryLock() {
		t.Fatal("network holds manager lock")
	}
	a.m.mu.Unlock()
	latestOffer(t, a, a.publish(t, p, replicaRequest()))
	if len(latestTake(t, a)) == 0 {
		t.Fatal("local publication blocked/lost")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type latestAdmissionStore struct {
	replication.Store
	err error
}

func (s latestAdmissionStore) Admit(ctx context.Context, item replication.Item, check func(context.Context) error) error {
	if item.Entry.Kind == replication.Record {
		return s.err
	}
	return s.Store.Admit(ctx, item, check)
}

func TestReplicaLatestDispatcherLatchesOnlyIncomingUncertainAdmission(t *testing.T) {
	for _, outcome := range []string{"unknown", "record-window", "guard-not-entered"} {
		t.Run(outcome, func(t *testing.T) {
			deferred := outcome != "unknown"
			a, b := newReplicaPair(t)
			startReplicaPair(t, a, b)
			p := a.publisher(t, 0, 1)
			latestOffer(t, a, a.publish(t, p, replicaRequest()))
			items := latestTake(t, a)
			peer := b.m.replica.peers[a.cert.Principal]
			admissionErr := errors.New("injected unclassified record admission failure")
			if outcome == "record-window" {
				admissionErr = replication.ErrRecordWindowDeferred
			} else if outcome == "guard-not-entered" {
				admissionErr = replication.AdmissionNotStarted(replication.ErrDenied)
			}
			service, err := replication.New(replication.Config{
				Endpoint: b.m.replicaEndpoint(), Store: latestAdmissionStore{Store: peer.store, err: admissionErr},
				IncomingPeer: b.transport().IncomingPeer, Dial: b.transport().Dial,
				Authorize: b.m.replicaAccess, Current: b.m.replicaCurrent, RemoteAuthority: peer.store.RemoteAuthority,
			})
			if err != nil {
				t.Fatal(err)
			}
			peer.service = service
			connection, err := a.transport().Dial(context.Background(), b.cert.Principal)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close.Close()
			result, err := a.m.SendReplicaLatest(context.Background(), b.cert.Principal, connection, items)
			if err == nil || result.Complete || result.Deferred != deferred || result.UnknownOutcome == deferred || b.m.replica.failed.Load() == deferred || a.m.replica.failed.Load() {
				t.Fatalf("wrong outcome/latch: %+v %v receiver=%t sender=%t", result, err, b.m.replica.failed.Load(), a.m.replica.failed.Load())
			}
			if len(latestTake(t, a)) != 0 {
				t.Fatal("failed incoming batch requeued")
			}
		})
	}
}

func TestReplicaLatestLinuxOrdinaryAcceptanceHook(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ordinary acceptance hook requires real Linux peer credentials")
	}
	a, b := newReplicaPair(t)
	startReplicaPair(t, a, b)
	g := a.config.Policy.Grants[0]
	mount, err := a.m.Ensure(context.Background(), g.App, g.Service, 1, g.Publish)
	if err != nil {
		t.Fatal(err)
	}
	client := moduleDial(t, mount.SocketPath)
	request := moduleRequest("gas")
	request.TtlNs = uint64(2 * time.Second)
	reply := modulePublish(t, client, request)
	latestRecord(t, latestTake(t, a), reply.SignedRecordJson)
	// The sink rejects this payload after signing; it must never enter the queue.
	request.PayloadJson = []byte(`{"unexpected":1}`)
	if _, err = client.Publish(context.Background(), request); err == nil {
		t.Fatal("invalid publication accepted")
	}
	if len(latestTake(t, a)) != 0 {
		t.Fatal("failed admission offered")
	}
}
