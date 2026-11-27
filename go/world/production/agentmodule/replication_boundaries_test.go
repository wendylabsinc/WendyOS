package agentmodule

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"runtime"
	"sync"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
	"google.golang.org/protobuf/proto"
)

func TestReplicaModuleTLSRequiresBothUsagesAndExactCredential(t *testing.T) {
	a, b := newReplicaPair(t)
	startReplicaPair(t, a, b)
	for _, usages := range [][]x509.ExtKeyUsage{{x509.ExtKeyUsageClientAuth}, {x509.ExtKeyUsageServerAuth}, {x509.ExtKeyUsageAny}, {}} {
		leaf := *b.cert.Leaf
		leaf.ExtKeyUsage = usages
		bad := testidentity.Issue(t, &leaf, b.cert.Issuer, b.cert.Key.Public(), b.cert.IssuerKey)
		chain := [][]byte{bad.Raw, b.cert.Issuer.Raw}
		w, _ := a.clock.Now()
		if _, e := a.m.replicaCertificate(chain, w); e == nil {
			t.Fatalf("accepted usages %v", usages)
		}
	}
	if _, e := a.m.AuthenticateReplicaPeer(context.Background(), a.cert.ChainDER); e == nil {
		t.Fatal("local identity accepted as peer")
	}
	other := testidentity.NewWith(t, moduleTestNow, b.cert.Principal)
	if _, e := a.m.AuthenticateReplicaPeer(context.Background(), other.ChainDER); e == nil {
		t.Fatal("same principal with uncommissioned credential accepted")
	}
	chain := cloneChain(b.cert.ChainDER)
	id, e := a.m.AuthenticateReplicaPeer(context.Background(), chain)
	if e != nil {
		t.Fatal(e)
	}
	chain[0][0] ^= 0xff
	if e = a.m.replicaCurrent(context.Background(), id, replication.Import, 1); e != nil {
		t.Fatal("caller mutated retained TLS bytes", e)
	}
	a.clock.mu.Lock()
	a.clock.window.Earliest = b.cert.NotAfter
	a.clock.window.Latest = b.cert.NotAfter.Add(time.Millisecond)
	a.clock.window.Monotonic += time.Hour
	a.clock.mu.Unlock()
	if e = a.m.replicaCurrent(context.Background(), id, replication.Import, 1); e == nil {
		t.Fatal("long-lived connection bypassed expiry")
	}
}

func TestReplicaModuleFinalGroupUsesOriginalDeadlines(t *testing.T) {
	for _, kind := range []string{"sample", "lease", "reader_replaced", "policy"} {
		t.Run(kind, func(t *testing.T) {
			a, b := newReplicaPair(t)
			startReplicaPair(t, a, b)
			p := a.publisher(t, 0, 1)
			original := a.publish(t, p, replicaRequest())
			b.publisher(t, 1, 1)
			b.sync(t)
			scope := b.config.Policy.scope(b.config.Policy.Grants[1], 1)
			// Store snapshots use their original SampleNow provider. This hook is
			// the final module sample read, after all remote proof/lease work.
			b.m.deps.SampleNow = func() time.Time {
				switch kind {
				case "sample":
					return moduleTestNow.Add(2 * time.Second)
				case "lease":
					b.clock.mu.Lock()
					b.clock.window.Earliest = moduleTestNow.Add(31 * time.Second)
					b.clock.window.Latest = moduleTestNow.Add(31*time.Second + time.Millisecond)
					b.clock.window.Monotonic += 31 * time.Second
					b.clock.mu.Unlock()
				case "reader_replaced":
					b.m.mu.Lock()
					b.m.bindings[bindingKey(scope.App, scope.Service)].scope.DeploymentGeneration++
					b.m.mu.Unlock()
				case "policy":
					b.clock.mu.Lock()
					b.clock.window.Earliest = b.config.Policy.NotAfter
					b.clock.window.Latest = b.config.Policy.NotAfter.Add(time.Millisecond)
					b.clock.window.Monotonic += time.Hour
					b.clock.mu.Unlock()
				}
				return moduleTestNow.Add(100 * time.Millisecond)
			}
			got, e := b.m.filterQueryCurrent(context.Background(), []serviceorigin.SignedRecord{original}, scope)
			if e == nil && len(got) != 0 {
				t.Fatal("final expiry or replacement returned original proof")
			}
		})
	}
}

func TestReplicaModuleRestartRetainsFloorsAndRefusesOldProcessLease(t *testing.T) {
	a, b := newReplicaPair(t)
	startReplicaPair(t, a, b)
	p := a.publisher(t, 0, 1)
	a.publish(t, p, replicaRequest())
	b.sync(t)
	// Simulated process loss: close disk handles without clean lifecycle floors.
	a.m.mu.Lock()
	a.m.closed = true
	a.m.mu.Unlock()
	a.m.cancel()
	_ = a.m.broker.Close()
	_ = a.m.closeReplicaStores()
	_ = a.m.journal.Close()
	a.config.CreateJournal = false
	a.config.CreateReplication = false
	var e error
	a.m, e = New(context.Background(), a.config, a.deps)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.m.StartReplication(context.Background(), a.transport()); e != nil {
		t.Fatal(e)
	}
	b.sync(t)
	if len(b.live(t)) != 0 {
		t.Fatal("old process lease survived restart")
	}
	states, e := a.m.replica.authority.Snapshot(context.Background())
	if e != nil || len(states) != 1 || states[0].MinimumEpoch != 2 {
		t.Fatalf("lost startup floor: %+v %v", states, e)
	}
	p = a.publisher(t, 0, 2)
	next := a.publish(t, p, replicaRequest())
	b.sync(t)
	got := b.live(t)
	if len(got) != 1 || !bytes.Equal(got[0].Record, next.Record) {
		t.Fatal("new process epoch unavailable")
	}
	lease, e := a.m.replica.authority.CurrentLease(context.Background(), sourceauthority.SourceOf(a.config.Policy.scope(a.config.Policy.Grants[0], 2)))
	if e != nil {
		t.Fatal(e)
	}
	var c sourceauthority.LeaseClaims
	_ = json.Unmarshal(lease.Claims, &c)
	if c.Epoch != 2 {
		t.Fatal("epoch reset on reopen")
	}
}

func TestReplicaModuleGuardMarkerExpiresAndPolicyLossStopsWork(t *testing.T) {
	a, b := newReplicaPair(t)
	startReplicaPair(t, a, b)
	var captured context.Context
	if e := a.m.withReplica(context.Background(), func(ctx context.Context) error {
		captured = ctx
		return a.m.withReplica(ctx, func(context.Context) error { return nil })
	}); e != nil {
		t.Fatal(e)
	}
	called := false
	if e := a.m.withReplica(captured, func(context.Context) error { called = true; return nil }); e == nil || called {
		t.Fatal("escaped fence marker accepted")
	}
	a.policyOK.Store(false)
	if _, e := a.m.AuthenticateReplicaPeer(context.Background(), b.cert.ChainDER); e == nil {
		t.Fatal("changed policy accepted")
	}
	a.policyOK.Store(true)
	if _, e := a.m.SyncPeer(context.Background(), b.cert.Principal); e == nil {
		t.Fatal("sticky policy fault reopened")
	}
}

func TestReplicaModuleConcurrentRepairReadAndRefreshKeepLockOrder(t *testing.T) {
	a, b := newReplicaPair(t)
	startReplicaPair(t, a, b)
	p := a.publisher(t, 0, 1)
	a.publish(t, p, replicaRequest())
	b.sync(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errors := make(chan error, 3)
	var wg sync.WaitGroup
	// Exactly three workers, each bounded to three operations and one context.
	launch := func(run func() error) { wg.Add(1); go func() { defer wg.Done(); errors <- run() }() }
	launch(func() error {
		for i := 0; i < 3; i++ {
			if e := a.m.RefreshReplication(ctx); e != nil {
				return e
			}
		}
		return nil
	})
	launch(func() error {
		for i := 0; i < 3; i++ {
			if _, e := b.m.SyncPeer(ctx, a.cert.Principal); e != nil {
				return e
			}
		}
		return nil
	})
	launch(func() error {
		for i := 0; i < 3; i++ {
			if _, e := b.m.replica.peers[a.cert.Principal].store.Snapshot(ctx); e != nil {
				return e
			}
		}
		return nil
	})
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("concurrent module operations did not release fences")
	}
	close(errors)
	for e := range errors {
		if e != nil {
			t.Fatal(e)
		}
	}
}

func TestReplicaModuleIdentityLossCancelsOutgoingDial(t *testing.T) {
	a, b := newReplicaPair(t)
	entered := make(chan struct{})
	transport := a.transport()
	transport.Dial = func(ctx context.Context, _ string) (replication.Connection, error) {
		close(entered)
		<-ctx.Done()
		return replication.Connection{}, ctx.Err()
	}
	if e := a.m.StartReplication(context.Background(), transport); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := a.m.SyncPeer(context.Background(), b.cert.Principal); done <- e }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("outgoing dial not entered")
	}
	a.identity.invalidate()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("identity loss acknowledged repair")
		}
	case <-time.After(time.Second):
		t.Fatal("identity loss left dial active")
	}
}

func TestReplicaModuleLinuxOrdinaryReaderQualifiedSources(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("actual ordinary reader requires Linux SO_PEERPIDFD")
	}
	a, b := newReplicaPair(t)
	startReplicaPair(t, a, b)
	for _, f := range []*moduleReplicaFixture{a, b} {
		g := f.config.Policy.Grants[0]
		mount, e := f.m.Ensure(context.Background(), g.App, g.Service, 1, g.Publish)
		if e != nil {
			t.Fatal(e)
		}
		client := moduleDial(t, mount.SocketPath)
		request, e := appv1.EncodePublish(replicaRequest())
		if e != nil {
			t.Fatal(e)
		}
		modulePublish(t, client, request)
	}
	b.sync(t)
	g := b.config.Policy.Grants[1]
	mount, e := b.m.EnsureAccess(context.Background(), g.App, g.Service, 1, nil, g.Subscribe)
	if e != nil {
		t.Fatal(e)
	}
	client := moduleDial(t, mount.SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	request := &appv1.QueryRequest{Sources: []*appv1.SourceSelector{accessSelector(g.Subscribe[0]), accessSelector(g.Subscribe[1])}, Limit: 1}
	first, e := client.Query(ctx, request)
	if e != nil || len(first.SignedRecordJson) != 1 || first.Next == "" {
		t.Fatalf("first qualified page: %+v %v", first, e)
	}
	next := proto.Clone(request).(*appv1.QueryRequest)
	next.After = first.Next
	second, e := client.Query(ctx, next)
	if e != nil || len(second.SignedRecordJson) != 1 {
		t.Fatalf("second qualified page: %+v %v", second, e)
	}
	devices := map[string]bool{}
	for _, raw := range append(first.SignedRecordJson, second.SignedRecordJson...) {
		var p serviceorigin.SignedRecord
		var r serviceorigin.Record
		_ = json.Unmarshal(raw, &p)
		_ = json.Unmarshal(p.Record, &r)
		devices[r.Scope.Device] = true
	}
	if len(devices) != 2 || !devices[a.cert.Principal] || !devices[b.cert.Principal] {
		t.Fatal("qualified source collapsed into local alias")
	}
	bad := proto.Clone(request).(*appv1.QueryRequest)
	bad.Sources[1].Device = "spiffe://wendy.sh/tenant/" + testidentity.Tenant + "/device/44444444-4444-4444-8444-444444444444"
	if out, e := client.Query(ctx, bad); e == nil || out != nil {
		t.Fatal("uncommissioned device returned data")
	}
	if out, e := client.GetSourceAuthority(ctx, sourceAuthorityRequest(g.Subscribe[1])); e == nil || out != nil {
		t.Fatal("remote source got local process authority")
	}
}
