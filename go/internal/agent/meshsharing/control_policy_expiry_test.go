package meshsharing

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

func TestGatewayControlExpiryProjectsBootPolicyRemainingWindow(t *testing.T) {
	factory, root := controlTestCredentialsWithRoot(t)
	original := factory(controlTestTenant, controlTestClient)
	peer := factory(controlTestTenant, controlTestGateway)
	leaf, err := x509.ParseCertificate(original.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(original.Signer.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: original.Certificate.Certificate[0]}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	floor := leaf.NotAfter.Add(-30 * time.Second)
	policy := certs.CertificateTimePolicy{Now: time.Now, Floor: func() time.Time { return floor }, Uncertain: func() bool { return true }}
	credentials, err := localmesh.NewCredentialsWithTimePolicy(certPEM, root, keyPEM, policy)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := credentials.Verify(peer.Certificate.Certificate, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	expires, err := controlExpiry(credentials, identity)
	if err != nil {
		t.Fatal(err)
	}
	remaining := expires.Sub(before)
	if remaining < 29*time.Second || remaining > 31*time.Second {
		t.Fatalf("control lifetime=%s, want remaining policy window30s (absolute leafNotAfter=%s, localfloor=%s)", remaining, leaf.NotAfter, floor)
	}
}

func TestPolicyControlOwnAndPeerSelectedWindows(t *testing.T) {
	for _, ownFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(ownFirst), func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			short, long := now.Add(time.Hour), now.Add(2*time.Hour)
			selfNA, peerNA := long, short
			if ownFirst {
				selfNA, peerNA = short, long
			}
			clock := &controlPolicyClock{floor: short.Add(-30 * time.Second), uncertain: true}
			self, peer := policyControlPair(t, clock, now.Add(-time.Hour), selfNA, now.Add(-time.Hour), peerNA)
			bounded, _ := policyControlConn(t, self, peer, nil)
			if remaining := time.Until(bounded.currentExpiry()); remaining < 29*time.Second || remaining > 31*time.Second {
				t.Fatalf("selected path remaining=%s", remaining)
			}
		})
	}
}

func TestPolicyControlClockCorrectionNeverExtendsBound(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	clock := &controlPolicyClock{floor: now.Add(time.Hour - 30*time.Second), uncertain: true}
	self, peer := policyControlPair(t, clock, now.Add(-time.Hour), now.Add(2*time.Hour), now.Add(-time.Hour), now.Add(time.Hour))
	bounded, _ := policyControlConn(t, self, peer, nil)
	initial := bounded.currentExpiry()
	clock.set(now.Add(time.Hour-10*time.Second), 0, true)
	shortened := bounded.currentExpiry()
	if !shortened.Before(initial.Add(-19 * time.Second)) {
		t.Fatalf("forward floor did not shorten %s -> %s", initial, shortened)
	}
	clock.set(now.Add(time.Hour-time.Minute), -10*time.Minute, true)
	if after := bounded.currentExpiry(); after.After(shortened) {
		t.Fatalf("backward correction extended %s -> %s", shortened, after)
	}
}

func TestPolicyControlIdleRetiresOwnAndPeerOnPolicyChange(t *testing.T) {
	for _, change := range []string{"own expiry", "peer expiry", "own certainty", "peer certainty"} {
		t.Run(change, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			selfNB, peerNB := now.Add(-time.Hour), now.Add(-time.Hour)
			selfNA, peerNA := now.Add(2*time.Hour), now.Add(2*time.Hour)
			target := now.Add(time.Hour)
			switch change {
			case "own expiry":
				selfNA = target
			case "peer expiry":
				peerNA = target
			case "own certainty":
				selfNB = now.Add(30 * time.Minute)
			case "peer certainty":
				peerNB = now.Add(30 * time.Minute)
			}
			clock := &controlPolicyClock{floor: now, uncertain: true}
			if strings.Contains(change, "expiry") {
				clock.floor = target.Add(-30 * time.Second)
			}
			self, peer := policyControlPair(t, clock, selfNB, selfNA, peerNB, peerNA)
			var closed atomic.Int32
			bounded, remote := policyControlConn(t, self, peer, func() { closed.Add(1) })
			readDone := make(chan error, 1)
			go func() { var b [1]byte; _, err := remote.Read(b[:]); readDone <- err }()
			if strings.Contains(change, "expiry") {
				clock.set(target.Add(time.Second), 0, true)
			} else {
				clock.set(now, 0, false)
			}
			select {
			case <-bounded.watchDone:
			case <-time.After(2 * time.Second):
				t.Fatal("idle credential policy change did not retire control")
			}
			if closed.Load() != 1 || time.Now().Before(bounded.currentExpiry()) {
				t.Fatal("invalid control retained live deadline")
			}
			select {
			case err := <-readDone:
				if err == nil {
					t.Fatal("closed control read succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("control remained open")
			}
			if err := bounded.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPolicyControlCloseJoinsWatcher(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	clock := &controlPolicyClock{floor: now, uncertain: true}
	self, peer := policyControlPair(t, clock, now.Add(-time.Hour), now.Add(time.Hour), now.Add(-time.Hour), now.Add(time.Hour))
	bounded, _ := policyControlConn(t, self, peer, nil)
	bounded.Close()
	select {
	case <-bounded.watchDone:
	default:
		t.Fatal("policy watcher survived Close")
	}
}

func TestPolicyControlServerGrantCanceledByOwnExpiry(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	ownEnd := now.Add(time.Hour)
	clock := &controlPolicyClock{floor: ownEnd.Add(-30 * time.Second), uncertain: true}
	client, gateway := policyControlPair(t, clock, now.Add(-time.Hour), now.Add(2*time.Hour), now.Add(-time.Hour), ownEnd)
	backend := newControlBackend()
	backend.grantDelay = time.Hour
	stop, raw, serverDone := controlPipe(t, gateway, client, backend, client.Device)
	defer stop()
	clientDone := make(chan error, 1)
	go func() {
		conn, _, err := negotiateGatewayLease(context.Background(), client, gateway.Device, raw)
		if conn != nil {
			conn.Close()
		}
		clientDone <- err
	}()
	limit := time.Now().Add(2 * time.Second)
	for backend.grants.Load() == 0 && time.Now().Before(limit) {
		time.Sleep(time.Millisecond)
	}
	if backend.grants.Load() != 1 {
		t.Fatal("grant did not start")
	}
	clock.set(ownEnd.Add(time.Second), 0, true)
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("own policy expiry did not cancel pending grant")
	}
	select {
	case err := <-clientDone:
		if err == nil {
			t.Fatal("expired grant succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("client negotiation survived expired grant")
	}
	if len(backend.granted) != 0 {
		t.Fatal("expired policy installed a grant")
	}
}

func TestPolicyControlFailedNegotiationJoinsWatcher(t *testing.T) {
	for _, failure := range []string{"invalid reply", "cancel before handoff"} {
		t.Run(failure, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			clock := &controlPolicyClock{floor: now, uncertain: true}
			client, gateway := policyControlPair(t, clock, now.Add(-time.Hour), now.Add(time.Hour), now.Add(-time.Hour), now.Add(time.Hour))
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			var serverDone <-chan error
			var raw net.Conn
			var stopServer context.CancelFunc
			if failure == "invalid reply" {
				left, right := net.Pipe()
				raw = left
				t.Cleanup(func() { left.Close(); right.Close() })
				done := make(chan error, 1)
				serverDone = done
				go func() {
					secure := tls.Server(right, controlTLS(gateway))
					if err := secure.HandshakeContext(ctx); err != nil {
						done <- err
						return
					}
					_, err := secure.Write([]byte{0, 0, 0})
					right.Close()
					done <- err
				}()
			} else {
				backend := newControlBackend()
				backend.grantDelay = time.Hour
				stopServer, raw, serverDone = controlPipe(t, gateway, client, backend, client.Device)
				defer stopServer()
				go func() {
					limit := time.Now().Add(2 * time.Second)
					for backend.grants.Load() == 0 && time.Now().Before(limit) {
						time.Sleep(time.Millisecond)
					}
					cancel()
				}()
			}
			conn, _, err := negotiateGatewayLease(ctx, client, gateway.Device, raw)
			if conn != nil || err == nil {
				t.Fatalf("%s unexpectedly handed off lease: %v", failure, err)
			}
			if stopServer != nil {
				stopServer()
			}
			select {
			case <-serverDone:
			case <-time.After(2 * time.Second):
				t.Fatal("server worker did not finish")
			}
			before := clock.calls.Load()
			time.Sleep(1200 * time.Millisecond)
			if after := clock.calls.Load(); after != before {
				t.Fatalf("policy watcher survived failed negotiation: calls %d -> %d", before, after)
			}
		})
	}
}
