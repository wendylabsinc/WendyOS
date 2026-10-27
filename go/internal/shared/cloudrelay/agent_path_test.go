package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
)

func TestPresenceSourceRemoval(t *testing.T) {
	for _, ip := range []string{"10.88.0.1", "fd2b:677a:9d11::1"} {
		p := &peer.Peer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}}
		if presenceSourceRemoved(p, func() ([]net.Addr, error) { return []net.Addr{&net.IPNet{IP: net.ParseIP(ip)}}, nil }) {
			t.Fatal("assigned source disconnected")
		}
		if presenceSourceRemoved(p, func() ([]net.Addr, error) { return nil, errors.New("unavailable") }) {
			t.Fatal("inspection error disconnected source")
		}
		if !presenceSourceRemoved(p, func() ([]net.Addr, error) { return []net.Addr{&net.IPNet{IP: net.ParseIP("192.0.2.1")}}, nil }) {
			t.Fatal("removed source retained")
		}
	}
	if presenceSourceRemoved(nil, nil) {
		t.Fatal("missing peer disconnected")
	}
}

func TestPresencePathRequiresStableReachableSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	local := net.ParseIP("192.0.2.1")
	p := &peer.Peer{LocalAddr: &net.TCPAddr{IP: local}, Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.100"), Port: 443}}
	checks, probes := 0, 0
	a := &Agent{sourceCheckInterval: time.Millisecond,
		sourceAddresses: func() ([]net.Addr, error) { return []net.Addr{&net.IPAddr{IP: local}}, nil },
		preferredSource: func(*net.TCPAddr) (net.IP, error) {
			checks++
			// First change flaps away; only the later stable change may probe.
			if checks == 2 {
				return local, nil
			}
			return net.ParseIP("192.0.2.2"), nil
		},
		pathReachable: func(context.Context, string) bool {
			probes++
			if checks < 4 {
				t.Fatal("probed an unstable route")
			}
			if probes == 2 && checks < 6 {
				t.Fatal("failed probe did not back off")
			}
			return probes == 2
		},
	}
	if err := a.watchPresencePath(ctx, p, "unused"); !errors.Is(err, errPresencePathMoved) {
		t.Fatalf("path never moved after successful probe: %v", err)
	}
	if probes != 2 {
		t.Fatalf("probes = %d", probes)
	}
}

func TestPresencePathRechecksSourceAfterProbe(t *testing.T) {
	for _, restored := range []bool{true, false} {
		t.Run(map[bool]string{true: "original route restored", false: "different route selected"}[restored], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			local := net.ParseIP("192.0.2.1")
			source := net.ParseIP("192.0.2.2")
			p := &peer.Peer{LocalAddr: &net.TCPAddr{IP: local}, Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.100"), Port: 443}}
			checks, probes := 0, 0
			a := &Agent{sourceCheckInterval: time.Millisecond,
				sourceAddresses: func() ([]net.Addr, error) { return []net.Addr{&net.IPAddr{IP: local}}, nil },
				preferredSource: func(*net.TCPAddr) (net.IP, error) {
					checks++
					if restored && checks >= 4 {
						cancel()
					}
					return source, nil
				},
				pathReachable: func(context.Context, string) bool {
					probes++
					if probes == 1 {
						// The broker probe finishes after routing has changed again.
						source = net.ParseIP("192.0.2.3")
						if restored {
							source = local
						}
					} else if checks < 5 {
						t.Fatal("replacement candidate did not stabilize before its probe")
					}
					return true
				},
			}
			err := a.watchPresencePath(ctx, p, "unused")
			if restored {
				if !errors.Is(err, context.Canceled) || probes != 1 {
					t.Fatalf("restored route disconnected presence: err=%v probes=%d", err, probes)
				}
			} else if !errors.Is(err, errPresencePathMoved) || probes != 2 {
				t.Fatalf("replacement route was not independently probed: err=%v probes=%d", err, probes)
			}
		})
	}
}

func TestPresencePathProbeBackoffDoesNotDelaySourceRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	local := net.ParseIP("192.0.2.1")
	p := &peer.Peer{LocalAddr: &net.TCPAddr{IP: local}, Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.100"), Port: 443}}
	checks := 0
	var probedAt []int
	a := &Agent{sourceCheckInterval: time.Millisecond,
		sourceAddresses: func() ([]net.Addr, error) {
			if checks >= 69 {
				return nil, nil
			}
			return []net.Addr{&net.IPAddr{IP: local}}, nil
		},
		preferredSource: func(*net.TCPAddr) (net.IP, error) {
			checks++
			return net.ParseIP("192.0.2.2"), nil
		},
		pathReachable: func(context.Context, string) bool {
			probedAt = append(probedAt, checks)
			return false
		},
	}
	if err := a.watchPresencePath(ctx, p, "unused"); !errors.Is(err, errPresenceSourceRemoved) {
		t.Fatalf("removed source survived probe backoff: %v", err)
	}
	if !slices.Equal(probedAt, []int{2, 4, 7, 12, 21, 38, 69}) {
		t.Fatalf("unexpected bounded probe schedule: %v", probedAt)
	}
}

// Exercise a real accepted gRPC presence stream, including its transport peer
// context. Removing its source must end presence without waiting for server
// traffic, renewal, or TCP retransmission, and keep the still-valid lease. A
// lease that expires before the reconnect is replaced, not re-presented.
func TestAgentEndsPresenceWhenPathChanges(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		removedSource, expired bool
	}{
		{"removed", true, false},
		{"moved", false, false},
		{"removed/expired lease", true, true},
		{"moved/expired lease", false, true},
	} {
		removedSource := tc.removedSource
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			v := &Verifier{Issuer: "https://cloud.test", fetched: time.Now(), keys: map[string]jwk{"test": {Kid: "test", Kty: "EC", Alg: "ES256", Crv: "P-256", X: b64.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: b64.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			r := &testRelay{t: t, endpoint: listener.Addr().String(), cloudKey: key, verifier: v, present: make(chan struct{}), offers: make(chan *pb.SessionOffer)}
			srv := grpc.NewServer()
			pb.RegisterTunnelAuthorizationServiceServer(srv, r)
			pb.RegisterTunnelBrokerV2ServiceServer(srv, r)
			go srv.Serve(listener)
			defer srv.Stop()
			dial := func(endpoint string) (*grpc.ClientConn, error) {
				return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
			}
			v.relayDial = dial
			var removed atomic.Bool
			inspected := make(chan struct{}, 1)
			a := &Agent{Endpoint: r.endpoint, Verifier: v, StateDir: t.TempDir(), Credentials: func() (string, string, []byte) { return "", "", nil }, Logger: zap.NewNop(), dialCloud: func(endpoint, _, _ string, _ []byte) (*grpc.ClientConn, error) { return dial(endpoint) }, sourceCheckInterval: 10 * time.Millisecond, sourceAddresses: func() ([]net.Addr, error) {
				if removedSource && removed.Load() {
					return nil, nil
				}
				select {
				case inspected <- struct{}{}:
				default:
				}
				return net.InterfaceAddrs()
			}}
			a.preferredSource = func(*net.TCPAddr) (net.IP, error) {
				if removed.Load() {
					return net.ParseIP("192.0.2.2"), nil
				}
				return net.ParseIP("127.0.0.1"), nil
			}
			// Probe the real gRPC fixture before moving the still-assigned source.
			done := make(chan error, 1)
			go func() { done <- a.runOnce(ctx) }()
			select {
			case <-r.present:
			case err := <-done:
				t.Fatalf("presence failed: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case <-inspected:
			case err := <-done:
				t.Fatalf("live source ended: %v", err)
			case <-ctx.Done():
				t.Fatal("transport source was not inspected")
			}
			removed.Store(true)
			select {
			case err := <-done:
				expected := errPresenceSourceRemoved
				if !removedSource {
					expected = errPresencePathMoved
				}
				if !errors.Is(err, expected) {
					t.Fatalf("wrong reconnect reason: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("removed source kept stale presence")
			}
			// The broker never refused the lease, so the reconnect presents
			// it again instead of asking Cloud for a new one.
			if a.lease == nil {
				t.Fatal("valid lease dropped on a path change")
			}
			if _, err := os.Stat(filepath.Join(a.StateDir, "lease.pb")); err != nil {
				t.Fatalf("persisted lease dropped: %v", err)
			}
			r.mu.Lock()
			issued, issues := r.lease.JTI, r.issues
			r.mu.Unlock()
			if tc.expired {
				a.leaseClaims.Exp = time.Now().Unix()
			}
			r.reconnected = make(chan struct{})
			removed.Store(false)
			go func() { done <- a.runOnce(ctx) }()
			select {
			case <-r.reconnected:
			case err := <-done:
				t.Fatalf("reconnect failed: %v", err)
			case <-ctx.Done():
				t.Fatal("presence did not reconnect")
			}
			cancel()
			<-done
			r.mu.Lock()
			defer r.mu.Unlock()
			if tc.expired {
				if r.issues-issues != 1 || r.lease.JTI == issued {
					t.Fatalf("expired lease reconnect issued %d leases, want 1", r.issues-issues)
				}
			} else if r.issues != issues || r.lease.JTI != issued {
				t.Fatal("path change requested a new lease while the current one was valid")
			}
		})
	}
}
