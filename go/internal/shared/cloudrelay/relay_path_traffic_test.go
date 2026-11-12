package cloudrelay

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// These are two real TCP forwarding paths, not a physical radio or kernel route
// move. Only route selection and the path watcher's source observation are
// injected; gRPC, signed leases/proofs, HPKE dial instructions, relay frames and
// destination TCP echo all run normally over plaintext loopback transports. The held Cloud socket models a stalled uplink.
func TestRelayTrafficSurvivesSlowRenewalAndPathMove(t *testing.T) {
	for _, pending := range []bool{true, false} {
		name := "acknowledged-renewal"
		if pending {
			name = "pending-renewal"
		}
		t.Run(name, func(t *testing.T) {
			var moved atomic.Bool
			var cloudDials atomic.Int32
			release, blocked, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce, blockedOnce sync.Once
			releaseSlow := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseSlow()
			var relay *testRelay
			var agent *Agent
			var oldPath, newPath *relayTCPPath
			lifecycle := &relayLifecycle{}
			lifecycle.configure = func(r *testRelay, a *Agent) {
				relay, agent = r, a
				r.renewWithSession = true
				r.renewed = make(chan struct{})
				r.presenceLeases = make(chan string, 4)
				r.renewAfter = func(issue int) time.Time {
					if issue == 1 {
						return time.Now().Add(200 * time.Millisecond)
					}
					return time.Now().Add(time.Minute)
				}
				oldPath = startRelayTCPPath(t, r.endpoint)
				newPath = startRelayTCPPath(t, r.endpoint)
				a.Verifier.relayDial = func(endpoint string) (*grpc.ClientConn, error) {
					path := oldPath
					if moved.Load() {
						path = newPath
					}
					return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, "tcp", path.listener.Addr().String())
					}))
				}
				a.dialCloud = func(endpoint, _, _ string, _ []byte) (*grpc.ClientConn, error) {
					call := cloudDials.Add(1)
					return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, target string) (net.Conn, error) {
						c, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
						if err != nil || call == 1 {
							return c, err
						}
						stalled := &heldRelayRead{Conn: c, release: release, blocked: func() { blockedOnce.Do(func() { close(blocked) }) }, done: make(chan struct{})}
						if call == 2 {
							stalled.closed = closed
						}
						return stalled, nil
					}))
				}
				a.sourceCheckInterval = 5 * time.Millisecond
				a.preferredSource = func(remote *net.TCPAddr) (net.IP, error) {
					if moved.Load() && remote.Port == oldPath.listener.Addr().(*net.TCPAddr).Port {
						return net.ParseIP("192.0.2.2"), nil
					}
					return net.ParseIP("127.0.0.1"), nil
				}
				// Keep the actual transport probe: five successful connections on
				// the replacement TCP path must precede the presence reconnect.
			}
			lifecycle.exercise = func(exchange func(string)) {
				awaitRelayPhase(t, blocked, "renewal transport never stalled")
				original := readRelayResume(t, agent)
				if <-relay.presenceLeases != original {
					t.Fatal("initial presence did not use persisted lease")
				}
				exchange("while renewal transport stalled")
				if !pending {
					releaseSlow()
					awaitRelayPhase(t, relay.renewed, "broker did not acknowledge renewal")
					deadline := time.Now().Add(2 * time.Second)
					for readRelayResume(t, agent) == original {
						if time.Now().After(deadline) {
							t.Fatal("acknowledged renewal was not persisted")
						}
						time.Sleep(time.Millisecond)
					}
					original = readRelayResume(t, agent)
					exchange("after acknowledged renewal")
				}
				moved.Store(true)
				awaitRelayPhase(t, relay.reconnected, "path move did not reconnect presence while renewal was pending")
				awaitRelayPhase(t, closed, "path move leaked the old renewal transport")
				if <-relay.presenceLeases != original {
					t.Fatal("replacement presence did not prove the retained lease")
				}
				if got := readRelayResume(t, agent); got != original {
					t.Fatal("path move replaced the valid persisted lease")
				}
				relay.mu.Lock()
				issues, joins, registrations := relay.issues, relay.joins, relay.registrations
				relay.mu.Unlock()
				wantIssues := 2
				if pending {
					wantIssues = 1
				}
				if issues != wantIssues || joins != 2 || registrations != 2 {
					t.Fatalf("path move changed lease/session ownership: issues=%d joins=%d presence=%d", issues, joins, registrations)
				}
				if newPath.accepted.Load() < 6 {
					t.Fatalf("replacement path did not carry five probes and presence: %d connections", newPath.accepted.Load())
				}
				oldBytes := oldPath.bytes.Load()
				exchange("same session after path move")
				if oldPath.bytes.Load() <= oldBytes {
					t.Fatal("original session did not continue carrying bytes on its original path")
				}
				if pending {
					releaseSlow()
					awaitRelayPhase(t, relay.renewed, "replacement presence could not renew after uplink recovered")
					exchange("same session after uplink recovery")
				}
			}
			testAuthorizedRelay(t, "agent", false, lifecycle)
		})
	}
}

func awaitRelayPhase(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal(failure)
	}
}

func readRelayResume(t *testing.T, a *Agent) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(a.StateDir, "lease.pb"))
	if err != nil {
		t.Fatal(err)
	}
	var lease pb.IssuePresenceLeaseResponse
	if err := proto.Unmarshal(raw, &lease); err != nil {
		t.Fatal(err)
	}
	return lease.PresenceLeaseJws
}

// Hold bytes read from an actual TCP socket until explicitly released. Closing
// the connection must unblock the held read, just as canceling an unavailable
// gRPC transport must drain its reader. No sleep determines the overlap.
type heldRelayRead struct {
	net.Conn
	release <-chan struct{}
	blocked func()
	done    chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *heldRelayRead) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.blocked()
		select {
		case <-c.release:
		case <-c.done:
			return 0, net.ErrClosed
		}
	}
	return n, err
}

func (c *heldRelayRead) Close() error {
	c.once.Do(func() {
		close(c.done)
		if c.closed != nil {
			close(c.closed)
		}
	})
	return c.Conn.Close()
}

type relayTCPPath struct {
	listener net.Listener
	accepted atomic.Int32
	bytes    atomic.Int64
}

type relayCountWriter struct {
	io.Writer
	bytes *atomic.Int64
}

func (w relayCountWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.bytes.Add(int64(n))
	return n, err
}

func startRelayTCPPath(t *testing.T, target string) *relayTCPPath {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	path := &relayTCPPath{listener: listener}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		var connections sync.WaitGroup
		defer connections.Wait()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			path.accepted.Add(1)
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer client.Close()
				stopClient := context.AfterFunc(ctx, func() { client.Close() })
				defer stopClient()
				server, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
				if err != nil {
					return
				}
				defer server.Close()
				stopServer := context.AfterFunc(ctx, func() { server.Close() })
				defer stopServer()
				upload := make(chan struct{})
				go func() {
					defer close(upload)
					_, _ = io.Copy(relayCountWriter{server, &path.bytes}, client)
					_ = server.Close()
				}()
				_, _ = io.Copy(relayCountWriter{client, &path.bytes}, server)
				_ = client.Close()
				<-upload
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		awaitRelayPhase(t, done, "TCP forwarding path did not drain")
	})
	return path
}
