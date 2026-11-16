package meshsession

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync/atomic"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
)

func TestCarrierGenerationUsesLaneLifecycle(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(map[bool]string{false: "reuse", true: "deferred"}[deferred], func(t *testing.T) {
			testDirectManagerReusesLiveCarrier(t, 0, deferred, true)
		})
	}
}

func testDirectManagerReusesLiveCarrier(t *testing.T, opener int, deferred bool, generation ...bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ca, cb := fixtureCredentials(t)
	endpoints := make(chan net.Conn, 1)
	var opens atomic.Int32
	backend := func(context.Context, meshid.Device, directstream.OpenRequest) (net.Conn, error) {
		opens.Add(1)
		x, y := net.Pipe()
		endpoints <- y
		return x, nil
	}
	a, err := NewDirectManager(ctx, DirectManagerConfig{Credentials: ca, Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := NewDirectManager(ctx, DirectManagerConfig{Credentials: cb, Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	cs, ss := directTestTLSWithCredentials(t, ca, cb)
	digest, _, err := ca.DirectSessionIdentity(cb.Device, cs)
	if err != nil {
		t.Fatal(err)
	}
	x, y := make(chan []byte, 4), make(chan []byte, 4)
	selected := directstream.CapabilityVersion | directstream.CapabilityTLS
	admissions := []localmesh.DirectCarrierAdmission{
		{Deferred: deferred, Self: ca.Device, Peer: cb.Device, Role: 0, IdentityDigest: digest, Selected: selected, TLS: cs, Records: &directTestCarrier{tx: x, rx: y}},
		{Deferred: deferred, Self: cb.Device, Peer: ca.Device, Role: 1, IdentityDigest: digest, Selected: selected, TLS: ss, Records: &directTestCarrier{tx: y, rx: x}},
	}
	managers := []*DirectManager{a, b}
	carrierDone := make(chan error, 2)
	for i, m := range managers {
		handler := m.HandleCarrier
		if len(generation) != 0 && generation[0] {
			var release func()
			handler, release, err = m.CarrierGeneration()
			if err != nil {
				t.Fatal(err)
			}
			defer release()
		}
		go func() { carrierDone <- handler(ctx, admissions[i]) }()
	}
	defer func() {
		cancel()
		for range 2 {
			select {
			case <-carrierDone:
			case <-time.After(time.Second):
				t.Error("carrier owner did not join")
			}
		}
	}()
	previous := make([]*DirectSession, 2)
	current := func(i int) *DirectSession {
		m := managers[i]
		m.mu.Lock()
		defer m.mu.Unlock()
		e := m.peers[admissions[i].Peer]
		if e == nil {
			return nil
		}
		return e.session
	}
	transfer := func(app, endpoint net.Conn, text string) {
		t.Helper()
		done := make(chan error, 1)
		go func() { _, err := app.Write([]byte(text)); done <- err }()
		got := make([]byte, len(text))
		if _, err := io.ReadFull(endpoint, got); err != nil || string(got) != text {
			t.Fatal(string(got), err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for cycle := 0; cycle < 3; cycle++ {
		for i := range managers {
			for {
				s := current(i)
				if s != nil && s != previous[i] && s.Status().Established && s.Status().DirectAvailable {
					if s.Status().BLEAlias != uint32(cycle+1) {
						t.Fatal("live carrier reused an attachment alias", s.Status())
					}
					previous[i] = s
					break
				}
				select {
				case err := <-carrierDone:
					t.Fatal("carrier ended", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
		}
		app, err := managers[opener].Dial(ctx, admissions[opener].Peer, directstream.OpenRequest{Service: directstream.ServiceTCP, Port: 28080})
		if err != nil {
			t.Fatal(err)
		}
		endpoint := <-endpoints
		deadline, _ := ctx.Deadline()
		app.SetDeadline(deadline)
		endpoint.SetDeadline(deadline)
		transfer(app, endpoint, "fresh direct epoch")
		if opens.Load() != int32(cycle+1) {
			t.Fatal("unexpected backend reopen", opens.Load())
		}
		if cycle == 2 {
			app.Close()
			endpoint.Close()
			break
		}
		cc, sc := directPromotionTLSCredentials(t, ca, cb, TLSALPN)
		cf, sf := directPromotionTLSCredentials(t, ca, cb, TLSALPN)
		control := []*tls.Conn{cc, sc}
		flow := []*tls.Conn{cf, sf}
		records := make([]*DirectUpperRecords, 2)
		promoted := make(chan struct{}, 2)
		done := make(chan error, 2)
		for i, m := range managers {
			records[i], err = NewDirectUpperRecords(ctx, control[i])
			if err != nil {
				t.Fatal(err)
			}
			pa := admissions[i]
			pa.Records = records[i]
			pa.TLS = control[i].ConnectionState()
			pa.PromotionInitiator = i == 0
			go func() {
				done <- m.HandlePromotion(ctx, pa, func(context.Context, DirectPromotionRequest) (DirectUpperFlow, error) {
					promoted <- struct{}{}
					return DirectUpperFlow{Stream: flow[i], TLS: flow[i].ConnectionState(), Initiator: i == 0}, nil
				})
			}()
		}
		for range 2 {
			waitLane(t, ctx, promoted)
		}
		transfer(app, endpoint, "same stream after promotion")
		app.Close()
		endpoint.Close()
		for _, s := range previous {
			waitLane(t, ctx, s.done)
		}
		for range 2 {
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		for _, r := range records {
			r.Close()
		}
	}
	if opens.Load() != 3 {
		t.Fatal("wrong deliberate application dial count", opens.Load())
	}
}

func waitLane(t *testing.T, ctx context.Context, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
