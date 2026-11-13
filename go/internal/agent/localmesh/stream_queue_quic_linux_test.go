//go:build linux

package localmesh

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

// Exercise the real QUIC/TLS stack over the production queue. A short quiet
// interval lets a datagram burst reach admission while the writer is occupied,
// as happens on BLE. The loopback socket supplies delivery after dequeue; this
// is an offline queue regression, not a radio or multihop qualification.
type queuedHandshakeSocket struct {
	net.PacketConn
	queue         *streamIPSendQueue
	hops          []*streamIPSendQueue
	drainInterval time.Duration
	source        byte
	sequence      atomic.Uint32
	lastWrite     atomic.Int64
	mu            sync.Mutex
	target        *net.UDPAddr
	stop          chan struct{}
	done          chan struct{}
	once          sync.Once
}

func newQueuedHandshakeSocket(t *testing.T, source byte, hops int, interval time.Duration, rate int) *queuedHandshakeSocket {
	t.Helper()
	raw, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &queuedHandshakeSocket{PacketConn: raw, queue: newStreamIPSendQueue(), source: source, stop: make(chan struct{}), done: make(chan struct{})}
	s.drainInterval = interval
	s.hops = []*streamIPSendQueue{s.queue}
	for len(s.hops) < hops {
		s.hops = append(s.hops, newStreamIPSendQueue())
	}
	for _, q := range s.hops {
		q.writeRateBytesPerSec = rate
	}
	go s.drain()
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func (s *queuedHandshakeSocket) WriteTo(data []byte, addr net.Addr) (int, error) {
	packet := queueTestAppUDP(48+len(data), s.source, 49200, AppSessionPort)
	copy(packet[48:], data)
	target := net.UDPAddrFromAddrPort(addr.(*net.UDPAddr).AddrPort())
	s.mu.Lock()
	s.target = target
	s.mu.Unlock()
	s.lastWrite.Store(time.Now().UnixNano())
	s.queue.enqueue(s.sequence.Add(1), packet, time.Now())
	return len(data), nil // IP overload is packet loss, not a socket error.
}
func (s *queuedHandshakeSocket) drain() {
	defer close(s.done)
	var workers sync.WaitGroup
	for index, q := range s.hops {
		workers.Add(1)
		go func() {
			defer workers.Done()
			tick := time.NewTicker(s.drainInterval)
			defer tick.Stop()
			served := 0
			for {
				select {
				case <-s.stop:
					return
				case at := <-tick.C:
					if index == 0 && at.Sub(time.Unix(0, s.lastWrite.Load())) < 10*time.Millisecond {
						continue
					}
					p, ok := q.pop(at, &served)
					if !ok {
						continue
					}
					if index+1 < len(s.hops) {
						s.hops[index+1].enqueue(p.sequence, p.data, at)
						continue
					}
					s.mu.Lock()
					target := s.target
					s.mu.Unlock()
					if target != nil {
						_, _ = s.PacketConn.WriteTo(p.data[48:], target)
					}
				}
			}
		}()
	}
	workers.Wait()
}
func (s *queuedHandshakeSocket) Close() error {
	var err error
	s.once.Do(func() { close(s.stop); <-s.done; err = s.PacketConn.Close() })
	return err
}

func TestStreamQueueFullCertificateQUICHandshake(t *testing.T) {
	testStreamQueueCertificateQUICHandshake(t, 1, 20*time.Millisecond, 0, 3*time.Second)
}

// Three independent production queues per direction model a slow BLE chain.
// This includes mutual large-chain TLS and an authenticated application echo;
// queue-only packet-count assertions cannot detect a stalled TLS crypto suffix.
// Radio scheduling, Babel and control-channel contention still need hardware.
func TestStreamQueueLowRateThreeHopMutualTLS(t *testing.T) {
	testStreamQueueCertificateQUICHandshake(t, 3, 250*time.Millisecond, 4096, 35*time.Second)
}

func testStreamQueueCertificateQUICHandshake(t *testing.T, hops int, interval time.Duration, rate int, idle time.Duration) {
	a, b := largeChainCredentials(t, 6500)
	clientTLS, err := a.PeerTLS(b.Device)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := b.PeerTLS(a.Device)
	if err != nil {
		t.Fatal(err)
	}
	config := func() *quic.Config {
		c := QUICConfig()
		c.InitialPacketSize = 1200
		c.DisablePathMTUDiscovery = true
		c.HandshakeIdleTimeout = idle
		return c
	}
	clientSocket := newQueuedHandshakeSocket(t, 1, hops, interval, rate)
	serverSocket := newQueuedHandshakeSocket(t, 2, hops, interval, rate)
	started := time.Now()
	t.Cleanup(func() {
		for side, socket := range map[string]*queuedHandshakeSocket{"client": clientSocket, "server": serverSocket} {
			for index, q := range socket.hops {
				t.Logf("elapsed=%s side=%s hop=%d queue=%+v", time.Since(started), side, index, q.snapshot())
			}
		}
	})
	clientTransport := &quic.Transport{Conn: clientSocket}
	serverTransport := &quic.Transport{Conn: serverSocket}
	defer clientTransport.Close()
	defer serverTransport.Close()
	listener, err := serverTransport.Listen(serverTLS, config())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*idle)
	defer cancel()
	type result struct {
		state tls.ConnectionState
		err   error
	}
	done := make(chan result, 1)
	go func() {
		c, e := listener.Accept(ctx)
		if e != nil {
			done <- result{err: e}
			return
		}
		stop := context.AfterFunc(ctx, func() { _ = c.CloseWithError(0, "test cancelled") })
		defer stop()
		stream, e := c.AcceptStream(ctx)
		var nonce [16]byte
		if e == nil {
			_, e = io.ReadFull(stream, nonce[:])
		}
		if e == nil {
			_, e = stream.Write(nonce[:])
		}
		done <- result{state: c.ConnectionState().TLS, err: e}
	}()
	conn, err := clientTransport.Dial(ctx, serverSocket.LocalAddr(), clientTLS, config())
	if err != nil {
		cancel()
		<-done
		t.Fatalf("full mutual QUIC handshake lost its queued prefix: %v; client=%+v server=%+v", err, clientSocket.queue.snapshot(), serverSocket.queue.snapshot())
	}
	defer conn.CloseWithError(0, "test complete")
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	_ = stream.SetDeadline(time.Now().Add(idle))
	nonce := [16]byte{1, 3, 5, 7, 9, 11, 13, 15}
	if _, err = stream.Write(nonce[:]); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	var echoed [16]byte
	if _, err = io.ReadFull(stream, echoed[:]); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	server := <-done
	if server.err != nil || echoed != nonce {
		t.Fatalf("authenticated echo failed: %v", server.err)
	}
	for _, state := range []tls.ConnectionState{conn.ConnectionState().TLS, server.state} {
		if !state.HandshakeComplete || state.DidResume || len(state.PeerCertificates) != 4 {
			t.Fatal("full mutual certificate verification was not exercised")
		}
	}
	if clientSocket.queue.snapshot().DroppedCapacity == 0 || serverSocket.queue.snapshot().DroppedCapacity == 0 {
		t.Fatal("test failed to exercise overloaded admission on both sides")
	}
}
