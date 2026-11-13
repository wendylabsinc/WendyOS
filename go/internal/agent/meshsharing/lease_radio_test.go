package meshsharing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// radioPath is one direction of an in-process mesh path: a one-way delay, a
// serialization rate, and segment loss that TCP repairs after a retransmission
// timeout, delaying every later byte as head-of-line blocking does.
type radioPath struct {
	oneWay, rto    time.Duration
	bytesPerSecond int
	loss           float64
}

// The field measured 0.5-1.3 s RTT and up to 25% loss over a Jetson BLE hop.
var bleHop = radioPath{oneWay: 300 * time.Millisecond, rto: time.Second, bytesPerSecond: 2048, loss: 0.25}

const radioSegment = 1220

// radioConn writes into an unbounded "socket buffer" and returns at once; a
// pump delivers each segment to the peer at its computed arrival time. Reads
// and read deadlines pass through to the underlying pipe.
type radioConn struct {
	net.Conn
	path   radioPath
	mu     sync.Mutex
	rng    *rand.Rand
	sentAt time.Time
	queue  chan radioFrame
	closed chan struct{}
	once   sync.Once
}

type radioFrame struct {
	data []byte
	at   time.Time
}

// radioPipe returns the borrower and gateway ends of a path. A fixed seed
// keeps loss reproducible.
func radioPipe(up, down radioPath) (*radioConn, *radioConn) {
	left, right := net.Pipe()
	return newRadioConn(left, up, 1), newRadioConn(right, down, 2)
}

func newRadioConn(conn net.Conn, path radioPath, seed uint64) *radioConn {
	c := &radioConn{Conn: conn, path: path, rng: rand.New(rand.NewPCG(seed, seed)), queue: make(chan radioFrame, 4096), closed: make(chan struct{})}
	go c.pump()
	return c
}

func (c *radioConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), radioSegment)
		send := time.Duration(n) * time.Second / time.Duration(c.path.bytesPerSecond)
		if c.path.loss > 0 && c.rng.Float64() < c.path.loss {
			send += c.path.rto
		}
		if now := time.Now(); now.After(c.sentAt) {
			c.sentAt = now
		}
		c.sentAt = c.sentAt.Add(send)
		c.queue <- radioFrame{data: bytes.Clone(p[:n]), at: c.sentAt.Add(c.path.oneWay)}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (c *radioConn) pump() {
	for {
		select {
		case frame := <-c.queue:
			select {
			case <-time.After(time.Until(frame.at)):
			case <-c.closed:
				return
			}
			if _, err := c.Conn.Write(frame.data); err != nil {
				return
			}
		case <-c.closed:
			return
		}
	}
}

func (c *radioConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// Writes never block, so only reads take deadlines.
func (c *radioConn) SetDeadline(t time.Time) error    { return c.Conn.SetReadDeadline(t) }
func (c *radioConn) SetWriteDeadline(time.Time) error { return nil }

// TLS 1.3 completes on the borrower before the gateway has even received the
// borrower's certificate flight. The upload, the gateway's grant and the
// reply then exceed the borrower's former fixed eight-second reply wait, so it
// abandoned leases the gateway was still granting.
func TestGatewayLeaseBorrowerWaitsForCertificateUploadAndGrant(t *testing.T) {
	t.Parallel()
	makeCreds := controlTestCredentials(t)
	client := makeCreds(controlTestTenant, controlTestClient)
	gateway := makeCreds(controlTestTenant, controlTestGateway)
	backend := newControlBackend()
	backend.grantDelay = 7500 * time.Millisecond // Within the gateway's own 8 s grant bound.
	path := radioPath{oneWay: 300 * time.Millisecond, bytesPerSecond: 2048}
	borrower, donor := radioPipe(path, path)
	defer borrower.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveGatewayLease(ctx, gateway, backend, controlAddressConn{donor, &net.TCPAddr{IP: net.IP(client.Device.MeshAddress().AsSlice()), Port: 12345}})
	}()
	conn, lease, err := negotiateGatewayLease(context.Background(), client, gateway.Device, borrower)
	if err != nil {
		t.Fatalf("borrower gave up on a lease the gateway was still granting: %v", err)
	}
	defer conn.Close()
	if !lease.valid() || <-backend.granted != client.Device {
		t.Fatal("wrong lease")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// On a BLE-like hop a lease negotiation outlasts a reconciliation step. The
// agent used to dial inside each 15 s step with an 8 s cap and discard the
// connection on timeout, so the handshake restarted forever. Short steps must
// instead collect one continuing negotiation.
func TestLeaseDialerFinishesSlowRadioNegotiationAcrossSteps(t *testing.T) {
	t.Parallel()
	makeCreds := controlTestCredentials(t)
	client := makeCreds(controlTestTenant, controlTestClient)
	gateway := makeCreds(controlTestTenant, controlTestGateway)
	backend := newControlBackend()
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	served := make(chan error, 4)
	var dials atomic.Int32
	dialer := leaseDialer{dial: func(ctx context.Context, peer meshid.Device) (net.Conn, tunnelLease, error) {
		dials.Add(1)
		borrower, donor := radioPipe(bleHop, bleHop)
		go func() {
			served <- serveGatewayLease(serverCtx, gateway, backend, controlAddressConn{donor, &net.TCPAddr{IP: net.IP(client.Device.MeshAddress().AsSlice()), Port: 12345}})
		}()
		ctx, cancel := context.WithTimeout(ctx, leaseDialTimeout)
		defer cancel()
		return negotiateGatewayLease(ctx, client, peer, borrower)
	}}
	defer dialer.stop()
	start, pending := time.Now(), 0
	var conn net.Conn
	for conn == nil {
		step, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		c, lease, err := dialer.await(step, gateway.Device)
		cancel()
		switch {
		case errors.Is(err, errGatewayPending):
			pending++
			if time.Since(start) > 30*time.Second {
				t.Fatal("negotiation never completed")
			}
		case err != nil:
			t.Fatal(err)
		case !lease.valid():
			t.Fatal("invalid lease")
		default:
			conn = c
		}
	}
	defer conn.Close()
	if pending < 2 {
		t.Fatalf("negotiation took %d pending steps; the path must outlast a step", pending)
	}
	if dials.Load() != 1 || backend.grants.Load() != 1 || <-backend.granted != client.Device {
		t.Fatalf("negotiation restarted: dials=%d grants=%d", dials.Load(), backend.grants.Load())
	}
	// The collected lease is live: one heartbeat crosses the slow path.
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := writeControl(conn, []byte{leaseHeartbeat}); err != nil {
		t.Fatal(err)
	}
	var ack [1]byte
	if _, err := io.ReadFull(conn, ack[:]); err != nil || ack[0] != leaseHeartbeat {
		t.Fatalf("heartbeat: %v %v", ack, err)
	}
	t.Logf("lease after %d pending steps in %v", pending, time.Since(start).Round(time.Millisecond))
	stopServer()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

// Withdrawal and a donor change must cancel a negotiation in flight, and a
// lease nobody collected must be closed so the gateway revokes it at once.
func TestLeaseDialerStopCancelsAndClosesUncollectedLease(t *testing.T) {
	type call struct {
		gateway   meshid.Device
		cancelled chan struct{}
	}
	calls := make(chan call, 4)
	release := make(chan struct{})
	var gatewaySide net.Conn
	dialer := leaseDialer{dial: func(ctx context.Context, gateway meshid.Device) (net.Conn, tunnelLease, error) {
		c := call{gateway, make(chan struct{})}
		calls <- c
		select {
		case <-release:
			borrower, donor := net.Pipe()
			gatewaySide = donor
			return borrower, tunnelLease{Slot: 7}, nil
		case <-ctx.Done():
			close(c.cancelled)
			return nil, tunnelLease{}, ctx.Err()
		}
	}}
	first, second := policyDonorDevice(t), mustPolicyDevice(t, policyTenant, policyDonorLate)
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := dialer.await(expired, first); !errors.Is(err, errGatewayPending) {
		t.Fatalf("expired step did not leave the negotiation pending: %v", err)
	}
	a := <-calls
	if _, _, err := dialer.await(expired, second); !errors.Is(err, errGatewayPending) {
		t.Fatalf("new donor: %v", err)
	}
	b := <-calls
	if a.gateway != first || b.gateway != second {
		t.Fatal("wrong donors dialed")
	}
	select {
	case <-a.cancelled:
	case <-time.After(time.Second):
		t.Fatal("donor change kept negotiating with the old donor")
	}
	close(release)
	<-dialer.pending.done
	dialer.stop()
	gatewaySide.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := gatewaySide.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("uncollected lease left open: %v", err)
	}
	if dialer.pending != nil {
		t.Fatal("stopped negotiation retained")
	}
}
