//go:build linux

package bleprovider

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Use real provisioned Credentials and the production BLE TLS configurations.
// The local stream replaces only the radio; every handshake and ticket is real.
func resumptionCredentials(t *testing.T) (issue func(meshid.Device) *localmesh.Credentials, high, low *localmesh.Credentials) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	serial := int64(1)
	issue = func(device meshid.Device) *localmesh.Credentials {
		t.Helper()
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, err := url.Parse(device.Principal())
		if err != nil {
			t.Fatal(err)
		}
		serial++
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, root, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(leafKey)
		if err != nil {
			t.Fatal(err)
		}
		credentials, err := localmesh.NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), chain, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
		if err != nil {
			t.Fatal(err)
		}
		return credentials
	}
	lowID := selfFixture.UUID()
	lowID[15]-- // Deliberately collide with the client's advertised UUID prefix.
	return issue, issue(selfFixture), issue(meshid.NewDevice(testTenant, lowID))
}

type resumptionState struct {
	client, server tls.ConnectionState
	err            error
}

func bleResumptionHandshake(t *testing.T, clientConfig, serverConfig *tls.Config) resumptionState {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	left, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	right, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	left.SetDeadline(time.Now().Add(3 * time.Second))
	right.SetDeadline(time.Now().Add(3 * time.Second))
	client := tls.Client(left, clientConfig)
	server := tls.Server(right, serverConfig)
	type serverResult struct {
		state tls.ConnectionState
		err   error
	}
	done := make(chan serverResult, 1)
	go func() {
		err := server.Handshake()
		if err == nil {
			_, err = server.Write([]byte{1})
		}
		done <- serverResult{server.ConnectionState(), err}
	}()
	err = client.Handshake()
	if err == nil {
		var one [1]byte
		_, err = io.ReadFull(client, one[:]) // Process the post-handshake session ticket.
	}
	left.Close() // Model a dropped stream, without graceful TLS shutdown.
	result := <-done
	if err == nil {
		err = result.err
	}
	return resumptionState{client.ConnectionState(), result.state, err}
}

func TestBLEFirstReconnectResumes(t *testing.T) {
	_, high, low := resumptionCredentials(t)
	client := &runtime{cfg: Config{Credentials: high}}
	server := (&runtime{cfg: Config{Credentials: low}}).makeServerTLS()
	peer := candidate{address: "02:00:00:00:00:01", addressType: "public", hint: deviceHint(low.Device.UUID())}
	first, authenticated, err := client.clientTLS(peer)
	if err != nil {
		t.Fatal(err)
	}
	if first.ClientSessionCache == nil {
		t.Fatal("first contact cannot collect a session ticket")
	}
	initial := bleResumptionHandshake(t, first, server)
	if initial.err != nil || initial.client.DidResume || initial.server.DidResume || *authenticated != low.Device {
		t.Fatalf("first contact: client resumed=%v server resumed=%v identity=%v error=%v", initial.client.DidResume, initial.server.DidResume, authenticated, initial.err)
	}
	pinned, err := high.PeerTLSWithTickets(low.Device, ALPN, "ble-tls")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pinned.ClientSessionCache.Get(pinned.ServerName); !ok {
		t.Fatal("first-contact ticket did not reach the canonical principal cache")
	}
	client.known = map[string]meshid.Device{peer.key(): *authenticated} // Production dialLink remembers this after TLS.
	for attempt := range 2 {
		reconnect, authenticated, err := client.clientTLS(peer)
		if err != nil {
			t.Fatal(err)
		}
		resumed := bleResumptionHandshake(t, reconnect, server)
		if resumed.err != nil || !resumed.client.DidResume || !resumed.server.DidResume || *authenticated != low.Device {
			t.Fatalf("reconnect %d: client resumed=%v server resumed=%v identity=%v error=%v", attempt+1, resumed.client.DidResume, resumed.server.DidResume, authenticated, resumed.err)
		}
	}
	// An unknown radio never offers a cached ticket, even for the same hint.
	rotated := peer
	rotated.address = "02:00:00:00:00:02"
	discovery, _, err := client.clientTLS(rotated)
	if err != nil {
		t.Fatal(err)
	}
	fresh := bleResumptionHandshake(t, discovery, server)
	if fresh.err != nil || fresh.client.DidResume || fresh.server.DidResume {
		t.Fatalf("unknown address reused an identity ticket: %v, resumed=%v", fresh.err, fresh.client.DidResume)
	}
}

func TestBLEFirstReconnectResumesAfterProviderRestart(t *testing.T) {
	issue, high, low := resumptionCredentials(t)
	client := &runtime{cfg: Config{Credentials: high}}
	peer := candidate{address: "02:00:00:00:00:01", addressType: "public", hint: deviceHint(low.Device.UUID())}
	config, authenticated, err := client.clientTLS(peer)
	if err != nil {
		t.Fatal(err)
	}
	initial := bleResumptionHandshake(t, config, (&runtime{cfg: Config{Credentials: low}}).makeServerTLS())
	if initial.err != nil || initial.client.DidResume || initial.server.DidResume || *authenticated != low.Device {
		t.Fatalf("first contact: %+v, identity=%v", initial, authenticated)
	}
	client.known = map[string]meshid.Device{peer.key(): low.Device}
	config, _, err = client.clientTLS(peer)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the BLE provider, as the supervisor does after adapter or
	// BlueZ recovery, while retaining the enrolled credentials.
	restarted := bleResumptionHandshake(t, config, (&runtime{cfg: Config{Credentials: low}}).makeServerTLS())
	if restarted.err != nil || !restarted.client.DidResume || !restarted.server.DidResume {
		t.Fatalf("provider restart discarded the first reconnect ticket: %+v", restarted)
	}
	// Reenrollment replaces the credential scope. It must still perform a
	// full handshake and present the new leaf even for the same principal.
	rotated := issue(low.Device)
	config, _, err = client.clientTLS(peer)
	if err != nil {
		t.Fatal(err)
	}
	fresh := bleResumptionHandshake(t, config, (&runtime{cfg: Config{Credentials: rotated}}).makeServerTLS())
	if fresh.err != nil || fresh.client.DidResume || fresh.server.DidResume ||
		len(fresh.client.PeerCertificates) == 0 || !bytes.Equal(fresh.client.PeerCertificates[0].Raw, rotated.Certificate.Certificate[0]) {
		t.Fatalf("rotated credentials reused the old ticket or leaf: %+v", fresh)
	}
}

func TestBLEFirstReconnectRevalidatesTrust(t *testing.T) {
	_, high, low := resumptionCredentials(t)
	var reject atomic.Bool
	verify := high.Verify
	high.Verify = func(chain [][]byte, now time.Time) (localmesh.Identity, error) {
		if reject.Load() {
			return localmesh.Identity{}, errors.New("identity no longer authorized")
		}
		return verify(chain, now)
	}
	client := &runtime{cfg: Config{Credentials: high}}
	server := (&runtime{cfg: Config{Credentials: low}}).makeServerTLS()
	peer := candidate{address: "02:00:00:00:00:01", addressType: "public", hint: deviceHint(low.Device.UUID())}
	config, authenticated, err := client.clientTLS(peer)
	if err != nil {
		t.Fatal(err)
	}
	if first := bleResumptionHandshake(t, config, server); first.err != nil {
		t.Fatal(first.err)
	}
	client.known = map[string]meshid.Device{peer.key(): *authenticated}
	reject.Store(true)
	reconnect, _, err := client.clientTLS(peer)
	if err != nil {
		t.Fatal(err)
	}
	result := bleResumptionHandshake(t, reconnect, server)
	if result.err == nil {
		t.Fatal("resumption bypassed current identity authorization")
	}
	if !result.client.DidResume {
		t.Fatal("test did not exercise an actual resumed handshake")
	}
}

func TestBLEFirstContactRejectsIdentityBeforeCaching(t *testing.T) {
	for _, name := range []string{"other tenant", "self", "wrong hint", "untrusted", "wrong ALPN", "missing ALPN"} {
		t.Run(name, func(t *testing.T) {
			issue, high, low := resumptionCredentials(t)
			client := &runtime{cfg: Config{Credentials: high}}
			server := (&runtime{cfg: Config{Credentials: low}}).makeServerTLS()
			peer := candidate{address: "02:00:00:00:00:01", addressType: "public", hint: deviceHint(low.Device.UUID())}
			switch name {
			case "other tenant":
				foreign := issue(meshid.NewDevice(otherTenant, low.Device.UUID()))
				server.Certificates = []tls.Certificate{foreign.Certificate}
			case "self":
				server.Certificates = []tls.Certificate{high.Certificate}
			case "wrong hint":
				peer.hint[0] ^= 1
			case "untrusted":
				high.Verify = func([][]byte, time.Time) (localmesh.Identity, error) {
					return localmesh.Identity{}, errors.New("untrusted peer")
				}
			case "wrong ALPN":
				server.NextProtos = []string{"another-protocol/1"}
			case "missing ALPN":
				server.NextProtos = nil
			}
			config, _, err := client.clientTLS(peer)
			if err != nil {
				t.Fatal(err)
			}
			capture, ok := config.ClientSessionCache.(*firstContactSessionCache)
			if !ok {
				t.Fatal("missing first-contact ticket collector")
			}
			result := bleResumptionHandshake(t, config, server)
			if name != "missing ALPN" && result.err == nil {
				t.Fatal("invalid first contact authenticated")
			}
			capture.mu.Lock()
			bound := capture.cache != nil
			capture.mu.Unlock()
			if bound {
				t.Fatal("invalid first contact bound a principal's session cache")
			}
			pinned, err := high.PeerTLSWithTickets(low.Device, ALPN, "ble-tls")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := pinned.ClientSessionCache.Get(pinned.ServerName); ok {
				t.Fatal("invalid first contact populated a verified peer's cache")
			}
		})
	}
}

func TestBLEFirstContactTicketScopeIsolation(t *testing.T) {
	_, high, low := resumptionCredentials(t)
	client := &runtime{cfg: Config{Credentials: high}}
	server := (&runtime{cfg: Config{Credentials: low}}).makeServerTLS()
	config, _, err := client.clientTLS(candidate{address: "02:00:00:00:00:01", addressType: "public", hint: deviceHint(low.Device.UUID())})
	if err != nil {
		t.Fatal(err)
	}
	if result := bleResumptionHandshake(t, config, server); result.err != nil {
		t.Fatal(result.err)
	}
	for _, scope := range []struct {
		name, alpn, transport string
		peer                  meshid.Device
	}{
		{"matching", ALPN, "ble-tls", low.Device},
		{"different protocol", ALPN + "-other", "ble-tls", low.Device},
		{"different transport", ALPN, "link-quic", low.Device},
		{"colliding hint", ALPN, "ble-tls", meshIDWithSameHint(low.Device)},
	} {
		t.Run(scope.name, func(t *testing.T) {
			pinned, err := high.PeerTLSWithTickets(scope.peer, scope.alpn, scope.transport)
			if err != nil {
				t.Fatal(err)
			}
			_, cached := pinned.ClientSessionCache.Get(pinned.ServerName)
			if cached != (scope.name == "matching") {
				t.Fatalf("cached=%v", cached)
			}
		})
	}
}

func meshIDWithSameHint(device meshid.Device) meshid.Device {
	id := device.UUID()
	id[15]--
	return meshid.NewDevice(device.Tenant(), id)
}
