package agentmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/protobuf/proto"
)

func sourceAuthorityRequest(sources ...serviceaccess.Source) *appv1.GetSourceAuthorityRequest {
	r := &appv1.GetSourceAuthorityRequest{Nonce: bytes.Repeat([]byte{42}, appv1.ClockNonceSize)}
	for _, s := range sources {
		r.Sources = append(r.Sources, accessSelector(s))
	}
	return r
}

func TestSourceAuthorityLinuxUsesPublicBindingWithoutObservationOrStores(t *testing.T) {
	f := setupModule(t, accessConfig)
	id := clockIdentityInstall(f)
	a := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	b := accessSource("telemetry-b", "wendy.sensor.scalar.v1", "gas-b")
	f.mount(t, 0, 11)
	f.mount(t, 1, 12)
	reader := moduleDial(t, accessMount(t, f, a, b).SocketPath)
	// No publication exists. These closures would make a replica/media path
	// unavailable; source authority must still make only its narrow assertion.
	f.manager.journal.Close()
	f.manager.evidence.Close()
	var samples atomic.Int32
	f.manager.deps.SampleNow = func() time.Time { samples.Add(1); return time.Time{} }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	q := sourceAuthorityRequest(a, b)
	out, e := reader.GetSourceAuthority(ctx, q)
	if e != nil || out == nil || len(out.Authorities) != 2 || samples.Load() != 0 || id.ordinary.Load() != 0 || id.clocks.Load() != 1 {
		t.Fatal("authority touched observation stores or ordinary fence", e)
	}
	if !bytes.Equal(out.Nonce, q.Nonce) {
		t.Fatal("nonce changed")
	}
	for i, source := range []serviceaccess.Source{a, b} {
		if !proto.Equal(out.Authorities[i].Source, q.Sources[i]) {
			t.Fatal("selector order/representation changed")
		}
		var delegation serviceorigin.SignedDelegation
		var claims serviceorigin.DelegationClaims
		if json.Unmarshal(out.Authorities[i].SignedDelegationJson, &delegation) != nil || json.Unmarshal(delegation.Claims, &claims) != nil || claims.Scope.App != source.App || claims.Scope.Service != source.Service || claims.Epoch == 0 {
			t.Fatal("invalid original public proof")
		}
		f.manager.mu.RLock()
		stored := bytes.Clone(f.manager.bindings[bindingKey(source.App, source.Service)].delegation)
		f.manager.mu.RUnlock()
		if !bytes.Equal(stored, out.Authorities[i].SignedDelegationJson) {
			t.Fatal("original delegation was reconstructed")
		}
	}
	if _, e = appv1.DecodeSourceAuthorityReply(out); e != nil {
		t.Fatal(e)
	}
	if v, e := reader.Query(ctx, &appv1.QueryRequest{Sources: q.Sources, Limit: 2}); e == nil || v != nil {
		t.Fatal("authority response falsely established replica availability")
	}
}

func TestSourceAuthorityLinuxExactLocalSubscriptionsAndCompleteSet(t *testing.T) {
	f := setupModule(t, accessConfig)
	a := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	b := accessSource("telemetry-b", "wendy.sensor.scalar.v1", "gas-b")
	f.mount(t, 0, 11)
	reader := moduleDial(t, accessMount(t, f, a, b).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	local := a
	local.Device = f.config.Policy.Device
	remote := a
	remote.Device = "spiffe://wendy.sh/tenant/" + f.config.Policy.Tenant + "/device/33333333-3333-4333-8333-333333333333"
	unauthorized := a
	unauthorized.Stream = "other"
	for _, q := range []*appv1.GetSourceAuthorityRequest{sourceAuthorityRequest(a, b), sourceAuthorityRequest(a, local), sourceAuthorityRequest(remote), sourceAuthorityRequest(unauthorized), sourceAuthorityRequest()} {
		if out, e := reader.GetSourceAuthority(ctx, q); e == nil || out != nil {
			t.Fatal("partial, alias, remote or unsubscribed source accepted")
		}
	}
	if out, e := reader.GetSourceAuthority(ctx, sourceAuthorityRequest(local)); e != nil || out == nil || out.Authorities[0].Source.Device != local.Device {
		t.Fatal("explicit local selector failed", e)
	}
	f.manager.Release(a.App, a.Service, 11)
	if out, e := reader.GetSourceAuthority(ctx, sourceAuthorityRequest(a)); e == nil || out != nil {
		t.Fatal("released source remained active")
	}
}

func TestSourceAuthorityLinuxSharedReadPassesWaitingDataOperation(t *testing.T) {
	f := setupModule(t, accessConfig)
	id := clockIdentityInstall(f)
	a := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	writer := moduleDial(t, f.mount(t, 0, 11).SocketPath)
	reader := moduleDial(t, accessMount(t, f, a).SocketPath)
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	defer unblock()
	id.before = func(clock bool) {
		if !clock {
			close(entered)
			<-resume
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	writeCtx, stopWrite := context.WithCancel(ctx)
	defer stopWrite()
	done := make(chan error, 1)
	go func() { _, e := writer.Publish(writeCtx, moduleRequest("gas")); done <- e }()
	finalReadAwait(t, entered)
	if out, e := reader.GetSourceAuthority(ctx, sourceAuthorityRequest(a)); e != nil || out == nil {
		t.Fatal("metadata queued behind data operation", e)
	}
	stopWrite()
	unblock()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("canceled write succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("data fence leaked")
	}
	if id.clocks.Load() != 1 || id.ordinary.Load() != 1 {
		t.Fatal("wrong fence dispatch")
	}
}

func TestSourceAuthorityLinuxFinalResponseRefusesSourceOrReaderChanges(t *testing.T) {
	for _, mode := range []string{"source-replaced", "source-released", "reader-replaced", "identity", "policy", "old-response", "clock-regression", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := setupModule(t, accessConfig)
			clockIdentityInstall(f)
			a := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
			f.mount(t, 0, 11)
			reader := moduleDial(t, accessMount(t, f, a).SocketPath)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, e := reader.GetSourceAuthority(ctx, sourceAuthorityRequest(a)); e != nil {
				t.Fatal(e)
			}
			original := f.manager.deps.PeerCheck
			entered, resume := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			var once sync.Once
			unblock := func() { once.Do(func() { close(resume) }) }
			defer unblock()
			f.manager.deps.PeerCheck = func(ctx context.Context, p appservice.Peer, scope serviceorigin.Scope) error {
				if e := original(ctx, p, scope); e != nil {
					return e
				}
				if scope.Service == "reader" && calls.Add(1) == 4 {
					close(entered)
					select {
					case <-resume:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}
			type result struct {
				out *appv1.SourceAuthorityReply
				err error
			}
			done := make(chan result, 1)
			go func() { out, e := reader.GetSourceAuthority(ctx, sourceAuthorityRequest(a)); done <- result{out, e} }()
			finalReadAwait(t, entered)
			switch mode {
			case "source-replaced":
				f.mount(t, 0, 12)
			case "source-released":
				f.manager.Release(a.App, a.Service, 11)
			case "reader-replaced":
				f.manager.Release("com.example.fusion", "reader", 7)
			case "identity":
				f.identity.invalidate()
			case "policy":
				f.policyOK.Store(false)
			case "old-response":
				finalReadAdvance(f.clock, appv1.MaxClockResponseAge+time.Nanosecond)
			case "clock-regression":
				finalReadAdvance(f.clock, -time.Millisecond)
			case "cancel":
				cancel()
			}
			unblock()
			select {
			case got := <-done:
				if got.err == nil || got.out != nil {
					t.Fatal("final authority change escaped")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("RPC did not finish")
			}
		})
	}
}

func sourceAuthorityFinalFixture(t *testing.T) (*Manager, *appv1.SourceAuthorityReply, *serviceorigin.Publisher, *moduleClock) {
	t.Helper()
	policy := basePolicy()
	id := testidentity.NewWith(t, moduleTestNow, policy.Device)
	clock := moduleTestClock()
	certs, e := wendycert.NewDevice(wendycert.Config{Tenant: policy.Tenant, Roots: [][]byte{id.RootDER}})
	if e != nil {
		t.Fatal(e)
	}
	a := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	readerScope := serviceorigin.Scope{Tenant: policy.Tenant, Site: policy.Site, Device: policy.Device, App: "com.example.reader", Service: "reader", DeploymentGeneration: 9, ExecutionDomain: "physical"}
	sourceScope := policy.scope(policy.Grants[0], 11)
	p, e := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: sourceScope, Epoch: 3, Lifetime: 10 * time.Minute, Clock: clock, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { p.Close() })
	d, e := p.PublicDelegation()
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(d)
	var claims serviceorigin.DelegationClaims
	if e = json.Unmarshal(d.Claims, &claims); e != nil {
		t.Fatal(e)
	}
	w, _ := clock.Now()
	device, e := certs.Verify(id.ChainDER, w.Earliest, w.Latest)
	if e != nil {
		t.Fatal(e)
	}
	from, until, _ := p.AuthorityWindow()
	finalReadAdvance(clock, until.Sub(w.Latest)-100*time.Millisecond)
	w, _ = clock.Now()
	m := &Manager{cfg: Config{Policy: policy}, deps: Dependencies{Identity: newModuleIdentity(id), Clock: clock, PolicyCurrent: func(context.Context) error { return nil }}, clockIncarnation: strings.Repeat("a", 32), bindings: map[string]*binding{}}
	m.bindings[bindingKey(sourceScope.App, sourceScope.Service)] = &binding{scope: sourceScope, token: 1, epoch: 3, authorityFrom: from, authorityUntil: until, delegation: raw, delegationClaims: claims, publisherDone: p.Done(), device: device}
	m.bindings[bindingKey(readerScope.App, readerScope.Service)] = &binding{scope: readerScope, subscribe: []serviceaccess.Source{a}, token: 1, authorityFrom: policy.NotBefore, authorityUntil: policy.NotAfter, device: device}
	reply := &appv1.SourceAuthorityReply{Nonce: bytes.Repeat([]byte{42}, appv1.ClockNonceSize), Authorities: []*appv1.SourceAuthority{{Source: accessSelector(a), SignedDelegationJson: bytes.Clone(raw)}}, Physical: &appv1.PhysicalClock{EarliestUnixNs: w.Earliest.UnixNano(), LatestUnixNs: w.Latest.UnixNano(), BootId: w.BootID, MonotonicNs: int64(w.Monotonic), Source: w.Source}, Tenant: readerScope.Tenant, Site: readerScope.Site, Device: readerScope.Device, App: readerScope.App, Service: readerScope.Service, ExecutionDomain: readerScope.ExecutionDomain, DeploymentGeneration: readerScope.DeploymentGeneration, ServerBootId: m.clockIncarnation}
	return m, reply, p, clock
}

func TestSourceAuthorityFinalCommonIntervalAndImmutableMetadata(t *testing.T) {
	for _, mode := range []string{"valid", "source-expiry", "reader-expiry", "policy-expiry", "source-done", "denied-key", "epoch", "bytes", "subscribe", "source-replaced"} {
		t.Run(mode, func(t *testing.T) {
			m, reply, p, clock := sourceAuthorityFinalFixture(t)
			scope := m.bindings[bindingKey(reply.App, reply.Service)].scope
			source := m.bindings[bindingKey(reply.Authorities[0].Source.App, reply.Authorities[0].Source.Service)]
			before, _ := proto.Marshal(reply)
			w, _ := clock.Now()
			switch mode {
			case "source-expiry":
				finalReadAdvance(clock, 100*time.Millisecond)
			case "reader-expiry":
				m.bindings[bindingKey(scope.App, scope.Service)].authorityUntil = w.Latest
			case "policy-expiry":
				m.cfg.Policy.NotAfter = w.Latest
			case "source-done":
				p.Close()
			case "denied-key":
				m.cfg.Policy.DeniedKeys = []string{source.device.SigningKeySHA256}
			case "epoch":
				source.epoch++
			case "bytes":
				reply.Authorities[0].SignedDelegationJson = append(reply.Authorities[0].SignedDelegationJson, ' ')
				before, _ = proto.Marshal(reply)
			case "subscribe":
				m.bindings[bindingKey(scope.App, scope.Service)].subscribe = nil
			case "source-replaced":
				source.scope.DeploymentGeneration++
			}
			if got := m.sourceAuthorityResponseCurrent(scope, reply); got != (mode == "valid") {
				t.Fatal("unexpected final authority", got)
			}
			after, _ := proto.Marshal(reply)
			if !bytes.Equal(before, after) {
				t.Fatal("finalizer refreshed original reply")
			}
		})
	}
}
