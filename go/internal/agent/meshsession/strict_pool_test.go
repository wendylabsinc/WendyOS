package meshsession

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

// Retiring a live connection leaves a per-peer barrier until logical QUIC
// closure completes. Concurrent new flows must wait, including when the old
// connection still has an active application stream.
func TestStrictPoolReplacementWaitsForConnectionClosure(t *testing.T) {
	a, b := fixtureCredentials(t)
	port := lifecycleEcho(t)
	address := lifecycleServer(t, b, allowPort(func(p uint16) bool { return p == port }), true)
	client := lifecycleClient(t, a)
	var dials atomic.Int32
	var previous atomic.Pointer[quic.Conn]
	replacementStarted := make(chan struct{}, 8)
	violations := make(chan error, 8)
	originalDial := client.dial
	client.dial = func(ctx context.Context, address netip.AddrPort, config *tls.Config, options *quic.Config) (*quic.Conn, error) {
		if dials.Add(1) > 1 {
			replacementStarted <- struct{}{}
			if old := previous.Load(); old != nil && old.Context().Err() == nil {
				violations <- errors.New("replacement handshake started while previous QUIC connection was alive")
			}
		}
		conn, err := originalDial(ctx, address, config, options)
		if err == nil {
			previous.Store(conn)
		}
		return conn, err
	}
	old := lifecycleDial(t, client, b.Device, address, port)
	client.mu.Lock()
	retirement := client.retireEntryLocked(client.peers[b.Device])
	client.mu.Unlock()
	if len(retirement) != 1 {
		t.Fatalf("retirement owners = %d", len(retirement))
	}
	var closeOnce sync.Once
	closeRetirement := func() { closeOnce.Do(func() { retirement[0].close("strict pool test replacement") }) }
	defer closeRetirement()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		conn net.Conn
		err  error
	}
	started, results := make(chan struct{}, 8), make(chan result, 8)
	for range 8 {
		go func() {
			started <- struct{}{}
			conn, err := client.Dial(ctx, b.Device, address, port)
			results <- result{conn, err}
		}()
	}
	for range 8 {
		<-started
	}
	select {
	case <-replacementStarted:
		t.Fatal("replacement bypassed the closing gate")
	case <-time.After(100 * time.Millisecond):
	}
	closeRetirement()
	if old.conn.Context().Err() == nil {
		t.Fatal("close returned while old QUIC connection was alive")
	}
	var replacement *quic.Conn
	for range 8 {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatal(result.err)
			}
			defer result.conn.Close()
			conn := result.conn.(*streamConn).conn
			if replacement == nil {
				replacement = conn
			}
			if conn != replacement || conn == old.conn {
				t.Fatal("concurrent waiters did not share one replacement connection")
			}
			lifecycleExchange(t, result.conn)
		case <-ctx.Done():
			t.Fatal("replacement waiters stalled", ctx.Err())
		}
	}
	if dials.Load() != 2 {
		t.Fatalf("initial plus replacement handshakes = %d", dials.Load())
	}
	select {
	case err := <-violations:
		t.Fatal(err)
	default:
	}
}

// A dial can finish its TLS handshake just as invalidation cancels setup. Keep
// that handshake's ready gate until the late returned connection is closed;
// otherwise another caller could establish a second outgoing QUIC connection.
func TestStrictPoolInvalidatedHandshakeClosesBeforeNextDial(t *testing.T) {
	for _, action := range []string{"invalidate", "withdraw"} {
		t.Run(action, func(t *testing.T) {
			a, b := fixtureCredentials(t)
			port := lifecycleEcho(t)
			address := lifecycleServer(t, b, allowPort(func(p uint16) bool { return p == port }), true)
			client := lifecycleClient(t, a)
			originalDial := client.dial
			var dials atomic.Int32
			var old atomic.Pointer[quic.Conn]
			established := make(chan *quic.Conn, 1)
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			replacementStarted := make(chan struct{}, 1)
			violations := make(chan error, 1)
			client.dial = func(ctx context.Context, address netip.AddrPort, config *tls.Config, options *quic.Config) (*quic.Conn, error) {
				if dials.Add(1) != 1 {
					replacementStarted <- struct{}{}
					if previous := old.Load(); previous != nil && previous.Context().Err() == nil {
						violations <- errors.New("new handshake overlapped invalidated late connection")
					}
					return originalDial(ctx, address, config, options)
				}
				conn, err := originalDial(ctx, address, config, options)
				if err != nil {
					return nil, err
				}
				old.Store(conn)
				established <- conn
				<-release // Intentionally return success after ignoring cancellation.
				return conn, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			type result struct {
				conn net.Conn
				err  error
			}
			first, next := make(chan result, 1), make(chan result, 1)
			go func() {
				conn, err := client.Dial(ctx, b.Device, address, port)
				first <- result{conn, err}
			}()
			select {
			case <-established:
			case <-ctx.Done():
				t.Fatal("first handshake did not establish")
			}
			if action == "invalidate" {
				client.Invalidate(b.Device)
			} else {
				client.WithdrawRoute(b.Device)
			}
			go func() {
				conn, err := client.Dial(ctx, b.Device, address, port)
				next <- result{conn, err}
			}()
			select {
			case <-replacementStarted:
				t.Fatal("next caller bypassed late-handshake cleanup")
			case <-time.After(100 * time.Millisecond):
			}
			close(release)
			select {
			case result := <-first:
				if result.conn != nil {
					result.conn.Close()
					t.Fatal("invalidated late handshake returned an app stream")
				}
				if !errors.Is(result.err, net.ErrClosed) && !errors.Is(result.err, context.Canceled) {
					t.Fatalf("invalidated handshake = %v", result.err)
				}
			case <-ctx.Done():
				t.Fatal("invalidated handshake cleanup stalled")
			}
			select {
			case result := <-next:
				if action == "withdraw" {
					if result.conn != nil {
						result.conn.Close()
						t.Fatal("queued caller bypassed the required fresh route check")
					}
					if !errors.Is(result.err, ErrNoRoute) || dials.Load() != 1 {
						t.Fatalf("queued caller after route withdrawal = %v, dials = %d", result.err, dials.Load())
					}
					// Only a new caller with fresh signed-route admission may
					// start again once the route has been restored.
					conn, err := client.Dial(ctx, b.Device, address, port)
					if err != nil {
						t.Fatal(err)
					}
					defer conn.Close()
					lifecycleExchange(t, conn)
				} else {
					if result.err != nil {
						t.Fatal(result.err)
					}
					defer result.conn.Close()
					lifecycleExchange(t, result.conn)
				}
			case <-ctx.Done():
				t.Fatal("new dial did not follow late connection cleanup")
			}
			if dials.Load() != 2 || old.Load().Context().Err() == nil {
				t.Fatal("late connection was not closed before exactly one replacement")
			}
			select {
			case err := <-violations:
				t.Fatal(err)
			default:
			}
		})
	}
}

// Keeping a transport through route loss does not weaken signed destination
// admission: a missing, withdrawn, expired, or changed listener proof closes an
// active session before another outgoing handshake can begin.
func TestStrictPoolDestinationProofInvalidation(t *testing.T) {
	for _, change := range []string{"expired", "withdrawn", "missing", "endpoint-change"} {
		t.Run(change, func(t *testing.T) {
			a, b := fixtureCredentials(t)
			port := lifecycleEcho(t)
			physical := lifecycleServer(t, b, allowPort(func(p uint16) bool { return p == port }), true)
			logical := netip.AddrPortFrom(b.Device.MeshAddress(), Port)
			client := lifecycleClient(t, a)
			originalDial := client.dial
			var dials atomic.Int32
			var previous atomic.Pointer[quic.Conn]
			violations := make(chan error, 1)
			client.dial = func(ctx context.Context, _ netip.AddrPort, config *tls.Config, options *quic.Config) (*quic.Conn, error) {
				if dials.Add(1) > 1 {
					if old := previous.Load(); old != nil && old.Context().Err() == nil {
						violations <- errors.New("proof replacement handshake overlapped the previous connection")
					}
				}
				conn, err := originalDial(ctx, physical, config, options)
				if err == nil {
					previous.Store(conn)
				}
				return conn, err
			}
			valid := func() localmesh.NodeSnapshot {
				return localmesh.NodeSnapshot{
					Routes: []babel.Route{{Prefix: netip.PrefixFrom(b.Device.MeshAddress(), 128)}},
					Devices: []localmesh.Manifest{{Device: b.Device.UUID(), Expires: time.Now().Add(time.Minute).UnixMilli(),
						AppEndpoints: []localmesh.AppEndpoint{{Transport: localmesh.AppQUIC, Port: Port}}}},
				}
			}
			old := lifecycleDial(t, client, b.Device, logical, port)
			lifecycleExchange(t, old)
			client.PruneRoutes(valid())
			if old.conn.Context().Err() != nil {
				t.Fatal("valid destination proof closed an active session")
			}
			snapshot := valid()
			switch change {
			case "expired":
				snapshot.Devices[0].Expires = time.Now().Add(-time.Second).UnixMilli()
			case "withdrawn":
				snapshot.Devices[0].Withdraw = true
			case "missing":
				snapshot.Devices = nil
			case "endpoint-change":
				snapshot.Devices[0].AppEndpoints[0].Port++
			}
			client.PruneRoutes(snapshot)
			if old.conn.Context().Err() == nil {
				t.Fatal("invalid signed destination proof retained an active connection")
			}
			// A later caller has obtained fresh valid admission again.
			client.PruneRoutes(valid())
			replacement := lifecycleDial(t, client, b.Device, logical, port)
			if replacement.conn == old.conn {
				t.Fatal("invalidated destination reused its old connection")
			}
			lifecycleExchange(t, replacement)
			if dials.Load() != 2 {
				t.Fatalf("initial plus replacement handshakes = %d", dials.Load())
			}
			select {
			case err := <-violations:
				t.Fatal(err)
			default:
			}
		})
	}
}

func TestStrictPoolWithdrawalDuringClosureRequiresFreshAdmission(t *testing.T) {
	a, b := fixtureCredentials(t)
	port := lifecycleEcho(t)
	address := lifecycleServer(t, b, allowPort(func(p uint16) bool { return p == port }), true)
	client := lifecycleClient(t, a)
	originalDial := client.dial
	var dials atomic.Int32
	var old atomic.Pointer[quic.Conn]
	violations := make(chan error, 1)
	client.dial = func(ctx context.Context, address netip.AddrPort, config *tls.Config, options *quic.Config) (*quic.Conn, error) {
		if dials.Add(1) > 1 {
			if conn := old.Load(); conn != nil && conn.Context().Err() == nil {
				violations <- errors.New("new admission started before prior connection closed")
			}
		}
		return originalDial(ctx, address, config, options)
	}
	first := lifecycleDial(t, client, b.Device, address, port)
	old.Store(first.conn)
	client.mu.Lock()
	retirement := client.retireEntryLocked(client.peers[b.Device])
	client.mu.Unlock()
	if len(retirement) != 1 {
		t.Fatalf("retirement owners = %d", len(retirement))
	}
	var closeOnce sync.Once
	closeRetirement := func() { closeOnce.Do(func() { retirement[0].close("withdraw during closure") }) }
	defer closeRetirement()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		conn net.Conn
		err  error
	}
	queued := make(chan result, 1)
	go func() {
		conn, err := client.Dial(ctx, b.Device, address, port)
		queued <- result{conn, err}
	}()
	select {
	case result := <-queued:
		if result.conn != nil {
			result.conn.Close()
		}
		t.Fatalf("caller bypassed the held closure: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}
	client.WithdrawRoute(b.Device)
	closeRetirement()
	select {
	case result := <-queued:
		if result.conn != nil {
			result.conn.Close()
			t.Fatal("queued caller proceeded without fresh route admission")
		}
		if !errors.Is(result.err, ErrNoRoute) || dials.Load() != 1 {
			t.Fatalf("queued withdrawal = %v, dials = %d", result.err, dials.Load())
		}
	case <-ctx.Done():
		t.Fatal("queued caller did not leave the closure barrier")
	}
	// The next call models freshly resolved admission after restoration; the
	// closed generation must not poison later valid callers.
	next := lifecycleDial(t, client, b.Device, address, port)
	lifecycleExchange(t, next)
	if dials.Load() != 2 {
		t.Fatalf("initial plus restored handshakes = %d", dials.Load())
	}
	select {
	case err := <-violations:
		t.Fatal(err)
	default:
	}
}

func TestStrictPoolCloseJoinsLateHandshakeAndConcurrentClose(t *testing.T) {
	a, b := fixtureCredentials(t)
	port := lifecycleEcho(t)
	address := lifecycleServer(t, b, allowPort(func(p uint16) bool { return p == port }), true)
	client := lifecycleClient(t, a)
	originalDial := client.dial
	established := make(chan *quic.Conn, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandshake := func() { releaseOnce.Do(func() { close(release) }) }
	// Run before lifecycleClient's registered cleanup so a failure cannot leave
	// Close waiting for this deliberately uncooperative dial wrapper.
	t.Cleanup(releaseHandshake)
	client.dial = func(ctx context.Context, address netip.AddrPort, config *tls.Config, options *quic.Config) (*quic.Conn, error) {
		conn, err := originalDial(ctx, address, config, options)
		if err != nil {
			return nil, err
		}
		established <- conn
		<-release // Ignore cancellation after establishing a live connection.
		return conn, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialResult := make(chan error, 1)
	go func() {
		conn, err := client.Dial(ctx, b.Device, address, port)
		if conn != nil {
			conn.Close()
			err = errors.New("closed client returned an application stream")
		}
		dialResult <- err
	}()
	var old *quic.Conn
	select {
	case old = <-established:
	case <-ctx.Done():
		t.Fatal("handshake did not establish")
	}
	closeResults := make(chan error, 2)
	for range 2 {
		go func() { closeResults <- client.Close() }()
	}
	select {
	case err := <-closeResults:
		t.Fatalf("Close returned before late handshake cleanup: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if old.Context().Err() != nil {
		t.Fatal("fixture did not hold a live late-returned connection")
	}
	releaseHandshake()
	for range 2 {
		select {
		case err := <-closeResults:
			if err != nil {
				t.Fatal(err)
			}
			if old.Context().Err() == nil {
				t.Fatal("Close returned while the late connection was still alive")
			}
		case <-ctx.Done():
			t.Fatal("concurrent Close did not join pending cleanup")
		}
	}
	select {
	case err := <-dialResult:
		if !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			t.Fatalf("closed pending dial = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("pending application dial did not return")
	}
}
