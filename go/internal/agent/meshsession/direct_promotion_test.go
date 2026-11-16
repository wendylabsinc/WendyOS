package meshsession

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func directPromotionTLS(t *testing.T) (*tls.Conn, *tls.Conn) {
	t.Helper()
	return directPromotionTLSProtocol(t, TLSALPN)
}

func directPromotionTLSProtocol(t *testing.T, protocol string) (*tls.Conn, *tls.Conn) {
	t.Helper()
	a, b := fixtureCredentials(t)
	return directPromotionTLSCredentials(t, a, b, protocol)
}

func directPromotionTLSCredentials(t *testing.T, a, b *localmesh.Credentials, protocol string) (*tls.Conn, *tls.Conn) {
	t.Helper()
	cc, err := a.PeerTLS(b.Device)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := b.PeerTLS(a.Device)
	if err != nil {
		t.Fatal(err)
	}
	a.ConfigureTLSClient(cc, protocol)
	sc.NextProtos = []string{protocol}
	b.ConfigureTLSServer(sc)
	x, y := net.Pipe()
	t.Cleanup(func() { x.Close(); y.Close() })
	client, server := tls.Client(x, cc), tls.Server(y, sc)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.HandshakeContext(ctx) }()
	if err = client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	return client, server
}

func TestDirectSessionPromotesExistingBackendAndSocket(t *testing.T) {
	for _, loser := range []int{-1, 0, 1, 2, 3, 4} {
		t.Run(fmt.Sprintf("superseded-side-%d", loser), func(t *testing.T) {
			testDirectSessionPromotesExistingBackendAndSocket(t, loser)
		})
	}
}
func TestDirectSessionRetiresDrainedPromotedEpoch(t *testing.T) {
	testDirectSessionPromotesExistingBackendAndSocket(t, -1, true)
}

func testDirectSessionPromotesExistingBackendAndSocket(t *testing.T, loser int, retirement ...bool) {
	retire := len(retirement) != 0 && retirement[0]
	selected := uint32(directstream.CapabilityVersion | directstream.CapabilityTLS)
	earlyLoss := loser >= 3
	if earlyLoss {
		loser -= 3
	}
	var opens atomic.Int32
	endpoints := make(chan net.Conn, 1)
	backend := func(context.Context, meshid.Device, directstream.OpenRequest) (net.Conn, error) {
		opens.Add(1)
		x, y := net.Pipe()
		endpoints <- y
		return x, nil
	}
	a, b := directTestSessions(t, backend, 3*time.Second)
	directTestAttachNegotiated(t, a, b, 4096, 1, selected)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	app, err := a.Dial(ctx, directstream.OpenRequest{Service: directstream.ServiceTCP, Port: 28080})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	endpoint := <-endpoints
	defer endpoint.Close()
	endpoint.SetDeadline(time.Now().Add(5 * time.Second))
	app.SetDeadline(time.Now().Add(5 * time.Second))
	// First prove the backend and original BLE lane work before handoff.
	if _, err = app.Write([]byte("before")); err != nil {
		t.Fatal(err)
	}
	var payload [6]byte
	if _, err = io.ReadFull(endpoint, payload[:]); err != nil || string(payload[:]) != "before" {
		t.Fatal(payload, err)
	}
	credentialsA, credentialsB := fixtureCredentials(t)
	client, server := directPromotionTLSCredentials(t, credentialsA, credentialsB, TLSALPN)
	controlClient, controlServer := directPromotionTLSCredentials(t, credentialsA, credentialsB, TLSALPN)
	controls := []*tls.Conn{controlClient, controlServer}
	records := make([]*DirectUpperRecords, 2)
	for i, control := range controls {
		records[i], err = NewDirectUpperRecords(ctx, control)
		if err != nil {
			t.Fatal(err)
		}
		defer records[i].Close()
	}
	upper := []*tls.Conn{client, server}
	calls := make(chan uint32, 2)
	promotionCtx, stop := context.WithCancel(context.Background())
	done := make(chan error, 2)
	defer func() {
		stop()
		a.Close()
		b.Close()
		for range 2 {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("promotion lifetime did not join")
			}
		}
	}()

	if loser >= 0 && loser < 2 {
		oldClient, oldServer := directPromotionTLSCredentials(t, credentialsA, credentialsB, TLSALPN)
		oldControl := []*tls.Conn{oldClient, oldServer}[loser]
		oldRecords, err := NewDirectUpperRecords(promotionCtx, oldControl)
		if err != nil {
			t.Fatal(err)
		}
		defer oldRecords.Close()
		losingSession := []*DirectSession{a, b}[loser]
		oldDone := make(chan error, 1)
		go func() {
			oldDone <- losingSession.Promote(promotionCtx, localmesh.DirectCarrierAdmission{
				PromotionInitiator: loser == 1, Peer: losingSession.cfg.Peer,
				IdentityDigest: losingSession.cfg.IdentityDigest, Role: losingSession.cfg.Role,
				Selected: selected,
				TLS:      oldControl.ConnectionState(), Records: oldRecords,
			}, func(context.Context, DirectPromotionRequest) (DirectUpperFlow, error) {
				return DirectUpperFlow{}, errors.New("losing control unexpectedly committed")
			})
		}()
		for !losingSession.Status().Promoting {
			select {
			case err := <-oldDone:
				t.Fatal("losing candidate rejected early", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
		if earlyLoss {
			_ = oldControl.NetConn().Close()
			select {
			case <-oldDone:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			for losingSession.Status().Promoting {
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
			status := losingSession.Status()
			if status.Closed || !status.UpperOwned {
				t.Fatalf("precommit loss changed retained upper ownership: %+v", status)
			}
		} else {
			defer func() {
				select {
				case err := <-oldDone:
					if !errors.Is(err, directstream.ErrPromoted) {
						t.Error("losing candidate", err)
					}
				case <-time.After(time.Second):
					t.Error("losing candidate did not join")
				}
			}()
		}
	}
	for i, s := range []*DirectSession{a, b} {
		i, s := i, s
		admission := localmesh.DirectCarrierAdmission{PromotionInitiator: (i == 0) != (loser == 2), Peer: s.cfg.Peer, IdentityDigest: s.cfg.IdentityDigest, Role: s.cfg.Role, Selected: selected, TLS: controls[i].ConnectionState(), Records: records[i]}
		go func() {
			done <- s.Promote(promotionCtx, admission, func(context.Context, DirectPromotionRequest) (DirectUpperFlow, error) {
				calls <- uint32(i)
				return DirectUpperFlow{Stream: upper[i], TLS: upper[i].ConnectionState(), Initiator: i == 0}, nil
			})
		}()
	}
	for range 2 {
		select {
		case <-calls:
		case err := <-done:
			t.Fatalf("promotion rejected: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if loser == 2 {
		// Even the preferred dial direction must not displace an already
		// committed opposite-direction connection or reset its app streams.
		lateClient, lateServer := directPromotionTLSCredentials(t, credentialsA, credentialsB, TLSALPN)
		for i, session := range []*DirectSession{a, b} {
			control := []*tls.Conn{lateClient, lateServer}[i]
			lateRecords, err := NewDirectUpperRecords(ctx, control)
			if err != nil {
				t.Fatal(err)
			}
			admission := localmesh.DirectCarrierAdmission{PromotionInitiator: i == 0,
				Peer: session.cfg.Peer, IdentityDigest: session.cfg.IdentityDigest, Role: session.cfg.Role,
				Selected: selected,
				TLS:      control.ConnectionState(), Records: lateRecords}
			err = session.Promote(ctx, admission, func(context.Context, DirectPromotionRequest) (DirectUpperFlow, error) {
				return DirectUpperFlow{}, errors.New("late candidate committed")
			})
			defer lateRecords.Close()
			if !errors.Is(err, directstream.ErrPromoted) {
				t.Fatal("late candidate", err)
			}
		}
	}
	if _, err = app.Write([]byte("after!")); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadFull(endpoint, payload[:]); err != nil || string(payload[:]) != "after!" {
		t.Fatal(payload, err)
	}
	response := make(chan error, 1)
	go func() { _, err := endpoint.Write([]byte("reply!")); response <- err }()
	if _, err = io.ReadFull(app, payload[:]); err != nil || string(payload[:]) != "reply!" {
		t.Fatal(payload, err)
	}
	if err = <-response; err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 1 {
		t.Fatalf("backend reopened %d times", opens.Load())
	}
	if retire {
		if a.Status().Closed || b.Status().Closed {
			t.Fatal("live app epoch retired")
		}
		app.Close()
		endpoint.Close()
		for _, session := range []*DirectSession{a, b} {
			select {
			case <-session.done:
			case <-ctx.Done():
				t.Fatal("idle promoted epoch remained alive", session.Status())
			}
		}
		if !errors.Is(context.Cause(a.ctx), errDirectEpochRetired) && !errors.Is(context.Cause(b.ctx), errDirectEpochRetired) {
			t.Fatal("neither peer completed orderly retirement", context.Cause(a.ctx), context.Cause(b.ctx))
		}
	}
}

type promotionObservedCarrier struct {
	*directTestCarrier
	ready chan struct{}
	once  atomic.Bool
}

func (c *promotionObservedCarrier) TrySendDirectRecord(p []byte) (bool, error) {
	ok, err := c.directTestCarrier.TrySendDirectRecord(p)
	if ok && p[0] == directstream.ControlCommitAck && c.once.CompareAndSwap(false, true) {
		close(c.ready)
	}
	return ok, err
}

func TestDirectSessionPromotesPendingOpen(t *testing.T) {
	for stage := 0; stage < 3; stage++ {
		t.Run([]string{"backend-still-opening", "result-lost-on-BLE", "open-lost-on-BLE"}[stage], func(t *testing.T) {
			lostResult := stage == 1
			entered, release := make(chan struct{}), make(chan struct{})
			endpoints := make(chan net.Conn, 1)
			var opens atomic.Int32
			backend := func(ctx context.Context, _ meshid.Device, _ directstream.OpenRequest) (net.Conn, error) {
				opens.Add(1)
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				x, y := net.Pipe()
				endpoints <- y
				return x, nil
			}
			a, b := directTestSessions(t, backend, 3*time.Second)
			oldSelf, oldPeer, _ := directTestAttach(t, a, b)
			if stage == 2 {
				oldSelf.dropRest.Store(true)
				oldSelf.drop.Store(uint32(directstream.FrameOpen))
			}
			if lostResult {
				oldPeer.dropRest.Store(true)
				oldPeer.drop.Store(uint32(directstream.FrameOpenResult))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			dial := make(chan directReply, 1)
			go func() {
				app, err := a.Dial(ctx, directstream.OpenRequest{Service: directstream.ServiceTCP, Port: 28080})
				dial <- directReply{app, err}
			}()
			initial := entered
			if stage == 2 {
				initial = oldSelf.dropped
			}
			select {
			case <-initial:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}

			if lostResult {
				close(release)
				select {
				case <-oldPeer.dropped:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			client, server := directPromotionTLS(t)
			upper := []*tls.Conn{client, server}
			tx, rx := make(chan []byte, 32), make(chan []byte, 32)
			ready := make(chan struct{})
			records := []*promotionObservedCarrier{
				{directTestCarrier: &directTestCarrier{tx: tx, rx: rx}, ready: make(chan struct{})},
				{directTestCarrier: &directTestCarrier{tx: rx, rx: tx}, ready: ready},
			}
			promotionCtx, stop := context.WithCancel(context.Background())
			done := make(chan error, 2)
			defer func() {
				stop()
				a.Close()
				b.Close()
				for range 2 {
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Error("promotion did not join")
					}
				}
			}()
			for i, s := range []*DirectSession{a, b} {
				i, s := i, s
				admission := localmesh.DirectCarrierAdmission{Peer: s.cfg.Peer, IdentityDigest: s.cfg.IdentityDigest, Role: s.cfg.Role, Selected: directstream.CapabilityVersion | directstream.CapabilityTLS, TLS: upper[i].ConnectionState(), Records: records[i]}
				go func() {
					done <- s.Promote(promotionCtx, admission, func(context.Context, DirectPromotionRequest) (DirectUpperFlow, error) {
						return DirectUpperFlow{Stream: upper[i], TLS: upper[i].ConnectionState(), Initiator: i == 0}, nil
					})
				}()
			}
			select {
			case <-ready:
			case err := <-done:
				done <- err
				t.Fatal(err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// Let the original backend finish only after the upper session commit.
			if !lostResult {
				close(release)
			}
			var reply directReply
			select {
			case reply = <-dial:
			case err := <-done:
				done <- err
				t.Fatal(err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if reply.err != nil {
				t.Fatal(reply.err)
			}
			defer reply.conn.Close()
			endpoint := <-endpoints
			defer endpoint.Close()
			endpoint.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := reply.conn.Write([]byte("ok")); err != nil {
				t.Fatal(err)
			}
			var payload [2]byte
			if _, err := io.ReadFull(endpoint, payload[:]); err != nil || string(payload[:]) != "ok" {
				t.Fatal(payload, err)
			}
			if opens.Load() != 1 {
				t.Fatal("duplicate backend", opens.Load())
			}
		})
	}
}

func TestDirectSessionPromotionFailureIsScopedToOneFlow(t *testing.T) {
	for _, cancelApp := range []bool{false, true} {
		t.Run(map[bool]string{false: "candidate-retry-then-cancellation", true: "app-cancellation"}[cancelApp], func(t *testing.T) {
			endpoints := make(chan net.Conn, 2)
			var opens atomic.Int32
			backend := func(context.Context, meshid.Device, directstream.OpenRequest) (net.Conn, error) {
				opens.Add(1)
				x, y := net.Pipe()
				endpoints <- y
				return x, nil
			}
			a, b := directTestSessions(t, backend, 3*time.Second)
			directTestAttach(t, a, b)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			bad, err := a.Dial(ctx, directstream.OpenRequest{Service: directstream.ServiceTCP, Port: 28080})
			if err != nil {
				t.Fatal(err)
			}
			defer bad.Close()
			badBackend := <-endpoints
			defer badBackend.Close()
			good, err := a.Dial(ctx, directstream.OpenRequest{Service: directstream.ServiceTCP, Port: 28081})
			if err != nil {
				t.Fatal(err)
			}
			defer good.Close()
			goodBackend := <-endpoints
			defer goodBackend.Close()
			client, server := directPromotionTLS(t)
			upper := []*tls.Conn{client, server}
			tx, rx := make(chan []byte, 32), make(chan []byte, 32)
			records := []*directTestCarrier{{tx: tx, rx: rx}, {tx: rx, rx: tx}}
			peerStarted := make(chan struct{})
			localStarted := make(chan struct{})
			retried := make(chan struct{})
			var attempts atomic.Int32
			peerCancelled := make(chan struct{})
			promotionCtx, stop := context.WithCancel(context.Background())
			done := make(chan error, 2)
			defer func() {
				stop()
				a.Close()
				b.Close()
				for range 2 {
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Error("promotion did not join")
					}
				}
			}()
			for i, s := range []*DirectSession{a, b} {
				i, s := i, s
				admission := localmesh.DirectCarrierAdmission{Peer: s.cfg.Peer, IdentityDigest: s.cfg.IdentityDigest, Role: s.cfg.Role, Selected: directstream.CapabilityVersion | directstream.CapabilityTLS, TLS: upper[i].ConnectionState(), Records: records[i]}
				go func() {
					done <- s.Promote(promotionCtx, admission, func(openCtx context.Context, request DirectPromotionRequest) (DirectUpperFlow, error) {
						id := request.FlowID
						if id == 2 {
							if i == 1 {
								close(peerStarted)
								<-openCtx.Done()
								close(peerCancelled)
								return DirectUpperFlow{}, openCtx.Err()
							}
							switch attempts.Add(1) {
							case 1:
								close(localStarted)
							case 2:
								close(retried)
							}
							if cancelApp {
								<-openCtx.Done()
								return DirectUpperFlow{}, openCtx.Err()
							}
							select {
							case <-peerStarted:
								return DirectUpperFlow{}, io.ErrUnexpectedEOF
							case <-openCtx.Done():
								return DirectUpperFlow{}, openCtx.Err()
							}
						}
						return DirectUpperFlow{Stream: upper[i], TLS: upper[i].ConnectionState(), Initiator: i == 0}, nil
					})
				}()
			}
			if cancelApp {
				for _, started := range []chan struct{}{localStarted, peerStarted} {
					select {
					case <-started:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
			} else {
				select {
				case <-retried:
				case <-ctx.Done():
					t.Fatal("failed candidate was not retried", ctx.Err())
				}
				bad.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
				var probe [1]byte
				if _, err := bad.Read(probe[:]); err == nil {
					t.Fatal("unexpected application data")
				} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
					t.Fatal("precommit failure closed application", err)
				}
			}
			bad.Close()
			select {
			case <-peerCancelled:
			case <-ctx.Done():
				t.Fatal("peer candidate was not cancelled", ctx.Err())
			}
			bad.SetReadDeadline(time.Now().Add(time.Second))
			var one [1]byte
			if _, err = bad.Read(one[:]); err == nil {
				t.Fatal("failed stream stayed open")
			}
			good.SetDeadline(time.Now().Add(time.Second))
			goodBackend.SetDeadline(time.Now().Add(time.Second))
			if _, err = good.Write([]byte("survive")); err != nil {
				t.Fatal("unrelated stream failed", err)
			}
			var payload [7]byte
			if _, err = io.ReadFull(goodBackend, payload[:]); err != nil || string(payload[:]) != "survive" {
				t.Fatal(payload, err)
			}
			for _, s := range []*DirectSession{a, b} {
				select {
				case <-s.done:
					t.Fatal("one flow killed session", context.Cause(s.ctx))
				default:
				}
			}
			if opens.Load() != 2 {
				t.Fatal("backend reopened", opens.Load())
			}
		})
	}
}

// Exercise negotiated TLS states for each upper ALPN. This checks admission
// policy, not transport-level QUIC migration or carrier failover.
