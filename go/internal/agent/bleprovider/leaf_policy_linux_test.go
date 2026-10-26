//go:build linux

package bleprovider

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestBLELeafAndRouterSharePhysicalAdapterBudget(t *testing.T) {
	policy := localmesh.NewConnectionPolicy(selfFixture, false, localmesh.ConnectionLimits{Outgoing: 2, Attempts: 1, Total: 3})
	queue := &localmesh.CandidateQueue{}
	router := &runtime{cfg: Config{Policy: policy, Candidates: queue}}
	peer := leafCandidate{config: localmesh.LeafBLEPeer{Device: lowFixture.UUID().String(), Address: "02:00:00:00:00:01", AddressType: "public"}, peer: lowFixture}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dialed := make(chan struct{}, 1)
	allow := make(chan struct{})
	ready := make(chan struct{}, 1)
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	leaf := &leafRuntime{policy: policy, candidates: queue, peers: []leafCandidate{peer}, dial: func(ctx context.Context, _ localmesh.LeafBLEPeer) (net.Conn, error) {
		dialed <- struct{}{}
		select {
		case <-allow:
			return left, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, serve: func(ctx context.Context, device meshid.Device, conn net.Conn) error {
		if !localmesh.ConnectionReady(ctx, device, func() { _ = conn.Close() }) {
			return errors.New("leaf admission rejected")
		}
		ready <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}
	defer func() { cancel(); leaf.workers.Wait() }()
	incoming := router.connectionPolicy().Reserve("incoming", peerFixture, false)
	if incoming == nil {
		t.Fatal("incoming reservation")
	}
	defer incoming.Release()
	pending := router.connectionPolicy().Reserve("router", testPeerDevice(), true)
	if pending == nil {
		t.Fatal("router setup")
	}
	now := time.Now()
	leaf.startCandidates(ctx, now)
	if len(dialed) != 0 || policy.Counts().Attempts != 1 {
		t.Fatal("leaf bypassed router's pending setup")
	}
	pending.Release()
	leaf.startCandidates(ctx, now)
	select {
	case <-dialed:
	case <-ctx.Done():
		t.Fatal("leaf dial did not start")
	}
	radio := candidate{address: "02:00:00:00:00:02", addressType: "public"}
	if lease := router.reserveCandidate(radio); lease != nil {
		lease.Release()
		t.Fatal("router bypassed leaf's pending setup")
	}
	close(allow)
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("leaf never became ready")
	}
	lease := router.reserveCandidate(radio)
	if lease == nil {
		t.Fatal("ready leaf retained setup capacity")
	}
	defer lease.Release()
	if !lease.Ready(testPeerDevice(), func() {}) {
		t.Fatal("router admission")
	}
	c := policy.Counts()
	if c.Outgoing != 2 || c.Attempts != 0 || c.Total != 3 {
		t.Fatalf("combined counts: %+v", c)
	}
	if extra := policy.Reserve("fourth", meshid.Device{}, false); extra != nil {
		extra.Release()
		t.Fatal("adapter total exceeded")
	}
	if extra := policy.Reserve("third-outgoing", meshid.Device{}, true); extra != nil {
		extra.Release()
		t.Fatal("adapter outgoing exceeded")
	}
	cancel()
	leaf.workers.Wait()
	if c := policy.Counts(); c.Total != 2 || c.Outgoing != 1 {
		t.Fatalf("leaf cancellation leaked reservation: %+v", c)
	}
}

func testPeerDevice() meshid.Device { return fixtureDevice("12345678-1234-4000-8000-000000000099") }

func TestBLELeafFailureReleasesBudgetAndSharesPenalty(t *testing.T) {
	policy := localmesh.NewConnectionPolicy(selfFixture, false, localmesh.ConnectionLimits{Outgoing: 2, Attempts: 1, Total: 3})
	queue := &localmesh.CandidateQueue{}
	peer := leafCandidate{config: localmesh.LeafBLEPeer{Device: lowFixture.UUID().String(), Address: "02:00:00:00:00:01", AddressType: "public"}, peer: lowFixture}
	leaf := &leafRuntime{policy: policy, candidates: queue, peers: []leafCandidate{peer}, dial: func(context.Context, localmesh.LeafBLEPeer) (net.Conn, error) {
		return nil, errors.New("time/bootstrap/dial failed")
	}}
	leaf.startCandidates(context.Background(), time.Now())
	leaf.workers.Wait()
	if c := policy.Counts(); c.Total != 0 {
		t.Fatalf("failed leaf leaked budget: %+v", c)
	}
	router := &runtime{cfg: Config{Policy: policy, Candidates: queue}}
	healthy := "public:02:00:00:00:00:02"
	for range 20 {
		if keys := router.candidateQueue().Order([]string{peer.key(), healthy}, time.Now()); keys[0] != healthy {
			t.Fatal("router ignored leaf failure history")
		}
	}
	// Address formatting must not create a second reservation for the same radio.
	duplicate := candidate{address: "02:00:00:00:00:01", addressType: " PUBLIC "}
	if duplicate.key() != peer.key() {
		t.Fatal("router/leaf radio keys differ")
	}
}

func TestBLELeafAndRouterShareDialPacing(t *testing.T) {
	policy := localmesh.NewConnectionPolicy(selfFixture, false, localmesh.ConnectionLimits{Outgoing: 2, Attempts: 1, Total: 3})
	pacer := &DialPacer{}
	router := &runtime{cfg: Config{Policy: policy, Pacer: pacer}}
	now := time.Now()
	if !router.admitDial(now) {
		t.Fatal("initial router start denied")
	}
	attempted := 0
	leaf := &leafRuntime{policy: policy, candidates: &localmesh.CandidateQueue{}, pacer: pacer, peers: []leafCandidate{{config: localmesh.LeafBLEPeer{Address: "02:00:00:00:00:01", AddressType: "public"}, peer: lowFixture}}, dial: func(context.Context, localmesh.LeafBLEPeer) (net.Conn, error) {
		attempted++
		return nil, errors.New("fixture dial")
	}}
	leaf.startCandidates(context.Background(), now.Add(time.Second))
	leaf.workers.Wait()
	if attempted != 0 || policy.Counts().Total != 0 {
		t.Fatal("leaf bypassed pacing or retained skipped reservation")
	}
	leaf.startCandidates(context.Background(), now.Add(globalDialPace))
	leaf.workers.Wait()
	if attempted != 1 || router.admitDial(now.Add(globalDialPace)) {
		t.Fatal("consumers did not share adapter pacing")
	}
}

// Exercise the production timestamp/TLS/pin/hello pipeline under a real
// adapter lease. A failure at each stage must close only that stream and leave
// capacity available to the router consumer on the next scheduling pass.
func TestBLELeafAdmissionFailuresReleaseSharedBudget(t *testing.T) {
	_, router, leaf := resumptionCredentials(t)
	authority := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("signed-time-response"))
	}))
	defer authority.Close()
	original := http.DefaultTransport
	http.DefaultTransport = authority.Client().Transport
	defer func() { http.DefaultTransport = original }()
	for _, stage := range []string{"timestamp", "TLS", "pin", "hello", "cancel"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			server, client := net.Pipe()
			defer client.Close()
			_ = client.SetDeadline(time.Now().Add(3 * time.Second))
			pinned := leaf.Device
			if stage == "pin" {
				pinned = peerFixture
			}
			policy := localmesh.NewConnectionPolicy(router.Device, false, localmesh.ConnectionLimits{Outgoing: 2, Attempts: 1, Total: 3})
			peer := leafCandidate{peer: pinned, config: localmesh.LeafBLEPeer{Device: pinned.UUID().String(), Address: "02:00:00:00:00:01", AddressType: "public"}}
			node := &localmesh.Node{Credentials: router}
			var serveErr error
			r := &leafRuntime{policy: policy, candidates: &localmesh.CandidateQueue{}, peers: []leafCandidate{peer},
				dial: func(context.Context, localmesh.LeafBLEPeer) (net.Conn, error) { return server, nil },
				serve: func(ctx context.Context, peer meshid.Device, conn net.Conn) error {
					serveErr = node.ServeLeafRadio(ctx, peer, conn, authority.URL)
					return serveErr
				}}
			r.startCandidates(ctx, time.Now())
			defer func() { cancel(); r.workers.Wait() }()
			var header [8]byte
			copy(header[:4], "WMT3")
			binary.BigEndian.PutUint32(header[4:], 1)
			if stage == "timestamp" {
				copy(header[:4], "WMT2")
			}
			if stage == "cancel" {
				cancel()
			} else {
				if _, err := client.Write(header[:]); err != nil {
					t.Fatal(err)
				}
				if stage != "timestamp" {
					if _, err := client.Write([]byte{0x30}); err != nil {
						t.Fatal(err)
					}
					if _, err := io.ReadFull(client, header[:4]); err != nil {
						t.Fatal(err)
					}
					if _, err := io.CopyN(io.Discard, client, int64(binary.BigEndian.Uint32(header[:4]))); err != nil {
						t.Fatal(err)
					}
					tenant, device := leaf.Device.Tenant().UUID(), leaf.Device.UUID()
					preface := append(append([]byte("WML3"), tenant[:]...), device[:]...)
					if _, err := client.Write(preface); err != nil {
						t.Fatal(err)
					}
					config, err := leaf.PeerTLS(router.Device)
					if err != nil {
						t.Fatal(err)
					}
					config.NextProtos = []string{localmesh.LeafALPN}
					if stage == "TLS" {
						config.NextProtos = []string{localmesh.StreamALPN}
					}
					secure := tls.Client(client, config)
					handshakeErr := secure.HandshakeContext(ctx)
					if stage == "TLS" && handshakeErr == nil {
						t.Fatal("router ALPN admitted on leaf carrier")
					}
					if stage != "TLS" && handshakeErr != nil {
						t.Fatal(handshakeErr)
					}
					if stage == "hello" {
						if _, err := localmesh.ReadControl(secure); err != nil {
							t.Fatal(err)
						}
						if err := localmesh.WriteControl(secure, localmesh.ControlMessage{Kind: "identity-request", Principal: leaf.Device.Principal()}); err != nil {
							t.Fatal(err)
						}
						_, _ = io.Copy(io.Discard, secure) // Drain the server TLS close.
					}
				}
			}
			r.workers.Wait()
			if serveErr == nil || policy.Counts().Total != 0 {
				t.Fatalf("failed admission retained capacity or succeeded: %v, %+v", serveErr, policy.Counts())
			}
			if stage == "pin" && !strings.Contains(serveErr.Error(), "pinned device") {
				t.Fatalf("did not reach the configured device pin: %v", serveErr)
			}
			if stage == "hello" && !strings.Contains(serveErr.Error(), "hello") {
				t.Fatalf("did not reach hello admission: %v", serveErr)
			}
			lease := policy.Reserve("router", peerFixture, true)
			if lease == nil {
				t.Fatal("leaf failure prevented the router from using released capacity")
			}
			lease.Release()
		})
	}
}
