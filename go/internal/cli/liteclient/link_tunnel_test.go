package liteclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/wcomrelaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// relayOpen is what a fakeRelay saw when a stream opened.
type relayOpen struct {
	assetID       string
	authorization []string
}

// fakeRelay transports opaque bytes to a device TLS server.
type fakeRelay struct {
	wcomrelaypb.UnimplementedWendyComRelayServiceServer
	refuse error
	grant  []byte
	opens  chan relayOpen
	ended  chan struct{}
	device func(net.Conn)
}

func newFakeRelay() *fakeRelay {
	return &fakeRelay{opens: make(chan relayOpen, 1), ended: make(chan struct{}, 1)}
}
func (r *fakeRelay) WendyComRelay(stream wcomrelaypb.WendyComRelayService_WendyComRelayServer) error {
	defer func() { r.ended <- struct{}{} }()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	md, _ := metadata.FromIncomingContext(stream.Context())
	r.opens <- relayOpen{assetID: first.GetOpen().GetRequestBody().GetTargetAssetId(), authorization: md.Get("authorization")}
	if r.refuse != nil {
		return r.refuse
	}
	if first.GetOpen().GetPrincipalRequest().GetKind() != "tunnel-principal-request+jws" {
		return status.Error(codes.PermissionDenied, "missing principal proof")
	}
	grant := r.grant
	if grant == nil {
		grant, err = relayGrantFixture()
		if err != nil {
			return err
		}
	}
	if err = stream.Send(&wcomrelaypb.WendyComRelayMessage{Msg: &wcomrelaypb.WendyComRelayMessage_Grant{Grant: &wcomrelaypb.WendyComRelayGrant{Record: grant}}}); err != nil {
		return err
	}
	proxy, device := net.Pipe()
	defer proxy.Close()
	defer device.Close()
	go r.device(device)
	done := make(chan error, 2)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				done <- err
				return
			}
			if msg.GetEstablished() != nil {
				continue
			}
			if _, err = proxy.Write(msg.GetPayload().GetBytes()); err != nil {
				done <- err
				return
			}
		}
	}()
	go func() {
		buffer := make([]byte, 137) // Deliberately split TLS records across relay payloads.
		for {
			n, err := proxy.Read(buffer)
			if err != nil {
				done <- err
				return
			}
			err = stream.Send(&wcomrelaypb.WendyComRelayMessage{Msg: &wcomrelaypb.WendyComRelayMessage_Payload{Payload: &wcomrelaypb.WendyComRelayPayload{Bytes: buffer[:n]}}})
			if err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		return err
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}

// dialFakeRelay serves relay in-process and returns a connection to it.
func dialFakeRelay(t *testing.T, relay *fakeRelay) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	wcomrelaypb.RegisterWendyComRelayServiceServer(srv, relay)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	cc, err := grpc.NewClient("passthrough:///relay",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return cc
}

const relayTestAssetID = "0b6f7a52-5d1e-4c3b-9a8e-2f4d6c8b1a3e"

func relayPKI(t *testing.T, principal string, trusted bool) (tls.Certificate, string, *tls.Config) {
	t.Helper()
	cert, chain, cfg, _, _ := relayPKIWithCA(t, principal, trusted)
	return cert, chain, cfg
}

// relayPKIWithCA also returns the session's issuing CA and key, so a test can
// hang a Lite Device CA under the trust the session already holds.
func relayPKIWithCA(t *testing.T, principal string, trusted bool) (tls.Certificate, string, *tls.Config, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	root, key := issueRelayCertificate(t, 1, "root", "", true, nil, nil)
	intermediate, ik := issueRelayCertificate(t, 2, "operator CA", "", true, root, key)
	operator, ok := issueRelayCertificate(t, 3, "operator", "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice", false, intermediate, ik)
	pool := x509.NewCertPool()
	pool.AddCert(root)
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: intermediate.Raw})) + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	if !trusted {
		root, key = issueRelayCertificate(t, 4, "untrusted", "", true, nil, nil)
	}
	device, dk := issueRelayCertificate(t, 5, "device", principal, false, root, key)
	return tls.Certificate{Certificate: [][]byte{operator.Raw, intermediate.Raw}, PrivateKey: ok}, chain, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{device.Raw}, PrivateKey: dk}}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}, intermediate, ik
}

// liteTestDevice is the enrolled board's device UUID, the only Lite pin.
const liteTestDevice = "00000000-0000-4000-8000-441bf6804ff8"

const devicePrincipal = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/" + liteTestDevice

func TestConnectViaRelayMTLS(t *testing.T) {
	for _, tc := range []struct {
		name, principal           string
		trusted, accept, fallback bool
		delay                     time.Duration
	}{
		{"matching device", devicePrincipal, true, true, false, 0},
		{"slow mesh relay", devicePrincipal, true, true, false, 11 * time.Second},
		{"wrong device", strings.Replace(devicePrincipal, "441bf6804ff8", "000000000000", 1), true, false, false, 0},
		{"wrong tenant", strings.Replace(devicePrincipal, "11111111-1111-4111-8111-111111111111", "33333333-3333-4333-8333-333333333333", 1), true, false, false, 0},
		{"untrusted issuer", devicePrincipal, false, false, false, 0},
		{"Lite CA under session trust", devicePrincipal, false, true, true, 0},
		{"self-signed Lite authority", devicePrincipal, false, false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, chain, cfg, sessionCA, sessionKey := relayPKIWithCA(t, tc.principal, tc.trusted)
			options := testRelayOptions(t, cert)
			fallbackCalls := 0
			if tc.fallback {
				// Accepted only when the Lite CA chains to the session's trust;
				// a self-signed authority is never an anchor.
				parent, parentKey := sessionCA, sessionKey
				if !tc.accept {
					parent, parentKey = nil, nil
				}
				lite, key := issueRelayCertificate(t, 10, "Lite Device CA", "", true, parent, parentKey)
				device, deviceKey := issueRelayCertificate(t, 11, "device", tc.principal, false, lite, key)
				cfg.Certificates = []tls.Certificate{{Certificate: [][]byte{device.Raw}, PrivateKey: deviceKey}}
				options.DeviceTrustFallback = func(context.Context) (string, error) {
					fallbackCalls++
					return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: lite.Raw})), nil
				}
			}
			relay := newFakeRelay()
			verified := make(chan error, 1)
			relay.device = func(raw net.Conn) {
				defer raw.Close()
				time.Sleep(tc.delay)
				raw.SetDeadline(time.Now().Add(5 * time.Second))
				conn := tls.Server(raw, cfg)
				err := conn.Handshake()
				verified <- err
				if err != nil {
					return
				}
				grant, readErr := relayGrantFixture()
				if readErr != nil {
					return
				}
				got := make([]byte, len(grant))
				if _, readErr = io.ReadFull(conn, got); readErr != nil || !bytes.Equal(got, grant) {
					return
				}
				if _, readErr = conn.Write([]byte("WLA2")); readErr != nil {
					return
				}
				link := newDirectLink(conn)
				msg, err := link.recv(5 * time.Second)
				if err != nil {
					return
				}
				if msg.GetHandshake() == nil {
					return
				}
				if err = link.send(msg); err != nil {
					return
				}
				io.Copy(io.Discard, conn)
			}
			cc := dialFakeRelay(t, relay)
			ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer session"), 30*time.Second)
			defer cancel()
			c := NewWendyLiteClient()
			err := c.ConnectViaRelay(ctx, cc, relayTestAssetID, cert, chain, liteTestDevice, options)
			if tc.accept {
				if err != nil {
					t.Fatal(err)
				}
				if err = <-verified; err != nil {
					t.Fatalf("operator mTLS: %v", err)
				}
				if c.PeerCertificate() == nil {
					t.Fatal("missing authenticated device certificate")
				}
				if err = c.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "device mTLS") {
				t.Fatalf("untrusted device accepted: %v", err)
			}
			if tc.fallback && fallbackCalls != 1 {
				t.Fatalf("device authority discovery calls = %d, want 1", fallbackCalls)
			}
			open := <-relay.opens
			if open.assetID != relayTestAssetID || len(open.authorization) != 1 || open.authorization[0] != "Bearer session" {
				t.Fatalf("lost caller context: %+v", open)
			}
			select {
			case <-relay.ended:
			case <-time.After(time.Second):
				t.Fatal("stream leaked")
			}
			if cc.GetState() != connectivity.Shutdown {
				t.Fatal("connection leaked")
			}
		})
	}
}

func TestConnectViaRelayReportsRefusalAndClosesConn(t *testing.T) {
	cert, chain, _ := relayPKI(t, devicePrincipal, true)
	relay := newFakeRelay()
	relay.refuse = status.Error(codes.PermissionDenied, "permission denied")
	cc := dialFakeRelay(t, relay)
	err := NewWendyLiteClient().ConnectViaRelay(context.Background(), cc, relayTestAssetID, cert, chain, liteTestDevice, testRelayOptions(t, cert))
	if err == nil || !strings.Contains(err.Error(), "PermissionDenied") {
		t.Fatalf("lost broker refusal: %v", err)
	}
	if cc.GetState() != connectivity.Shutdown {
		t.Fatal("connection leaked")
	}
}

func TestRelayRejectsMalformedGrantBeforeTLS(t *testing.T) {
	cert, chain, _ := relayPKI(t, devicePrincipal, true)
	current, err := relayGrantFixture()
	if err != nil {
		t.Fatal(err)
	}
	legacyHex, err := os.ReadFile("testdata/lite-grant-legacy.hex")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := hex.DecodeString(strings.TrimSpace(string(legacyHex)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		record func([]byte) []byte
	}{
		{"legacy grant missing current bindings", func([]byte) []byte { return legacy }},
		{"empty", func([]byte) []byte { return []byte{} }},
		{"truncated header", func(b []byte) []byte { return b[:4] }},
		{"wrong magic", func(b []byte) []byte { b[3] = '1'; return b }},
		{"truncated signature", func(b []byte) []byte { return b[:len(b)-1] }},
		{"trailing byte", func(b []byte) []byte { return append(b, 0) }},
		{"wrong key ID length", func(b []byte) []byte { b[4]++; return b }},
		{"empty key ID", func(b []byte) []byte { return relayGrantWithKeyID(b, "") }},
		{"oversized key ID", func(b []byte) []byte { return relayGrantWithKeyID(b, strings.Repeat("a", 129)) }},
		{"invalid key ID", func(b []byte) []byte { b[5] = '/'; return b }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			relay := newFakeRelay()
			relay.grant = tc.record(bytes.Clone(current))
			relay.device = func(conn net.Conn) {
				defer conn.Close()
				var b [1]byte
				if n, _ := conn.Read(b[:]); n != 0 {
					t.Error("sent TLS data after an invalid grant")
				}
			}
			cc := dialFakeRelay(t, relay)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client := NewWendyLiteClient()
			err := client.ConnectViaRelay(ctx, cc, relayTestAssetID, cert, chain, liteTestDevice, testRelayOptions(t, cert))
			if err == nil {
				client.Close()
				t.Fatal("accepted invalid grant")
			}
			if !strings.Contains(err.Error(), "no valid compact Lite grant") {
				t.Fatalf("unexpected rejection: %v", err)
			}
			select {
			case <-relay.ended:
			case <-time.After(time.Second):
				t.Fatal("invalid grant leaked stream")
			}
			if cc.GetState() != connectivity.Shutdown {
				t.Fatal("invalid grant leaked connection")
			}
		})
	}
}

func TestLiteGrantFramingKeyIDBounds(t *testing.T) {
	grant, err := relayGrantFixture()
	if err != nil {
		t.Fatal(err)
	}
	for _, kid := range []string{"a", strings.Repeat("a", 123) + "-_.Z9"} {
		// Changing the key ID invalidates the signature. The CLI checks framing;
		// only the device has the trusted key set and can authorize the grant.
		if !validLiteGrantRecord(relayGrantWithKeyID(grant, kid)) {
			t.Errorf("rejected supported %d-byte key ID", len(kid))
		}
	}
}

func TestLiteGrantFramingAcceptsOnlyWLG3(t *testing.T) {
	wlg3, err := relayGrantFixture()
	if err != nil {
		t.Fatal(err)
	}
	wlg2Hex, err := os.ReadFile("testdata/lite-grant-wlg2.hex")
	if err != nil {
		t.Fatal(err)
	}
	wlg2, err := hex.DecodeString(strings.TrimSpace(string(wlg2Hex)))
	if err != nil {
		t.Fatal(err)
	}
	if string(wlg3[:4]) != "WLG3" || string(wlg2[:4]) != "WLG2" {
		t.Fatal("fixtures carry the wrong magic")
	}
	if !validLiteGrantRecord(wlg3) {
		t.Error("rejected the WLG3 conformance record")
	}
	// No dual-accept window: a real WLG2 record, or WLG3 bytes under its magic, is refused.
	relabel := func(grant []byte, magic string) []byte { return append([]byte(magic), grant[4:]...) }
	if validLiteGrantRecord(wlg2) || validLiteGrantRecord(relabel(wlg3, "WLG2")) || validLiteGrantRecord(relabel(wlg2, "WLG3")) {
		t.Error("accepted a WLG2 or misframed record")
	}
}

func relayGrantWithKeyID(grant []byte, kid string) []byte {
	record := append(bytes.Clone(grant[:4]), byte(len(kid)))
	record = append(record, kid...)
	return append(record, grant[5+int(grant[4]):]...)
}

func TestRelayCancelWhileUnread(t *testing.T) {
	cert, _, _ := relayPKI(t, devicePrincipal, true)
	relay := newFakeRelay()
	relay.device = func(conn net.Conn) { defer conn.Close(); conn.Write(make([]byte, 65536)) }
	cc := dialFakeRelay(t, relay)
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := openRelayConn(ctx, cc, relayTestAssetID, testRelayOptions(t, cert))
	if err != nil {
		t.Fatal(err)
	}
	<-relay.opens
	cancel()
	select {
	case <-relay.ended:
	case <-time.After(time.Second):
		t.Fatal("cancel did not release blocked relay")
	}
	conn.Close()
	if cc.GetState() != connectivity.Shutdown {
		t.Fatal("connection leaked")
	}
}

func TestRelayReadDeadline(t *testing.T) {
	cert, _, _ := relayPKI(t, devicePrincipal, true)
	relay := newFakeRelay()
	relay.device = func(conn net.Conn) { defer conn.Close(); io.Copy(io.Discard, conn) }
	cc := dialFakeRelay(t, relay)
	conn, err := openRelayConn(context.Background(), cc, relayTestAssetID, testRelayOptions(t, cert))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	_, err = conn.Read(make([]byte, 1))
	if e, ok := err.(net.Error); !ok || !e.Timeout() {
		t.Fatalf("deadline ignored: %v", err)
	}
}

func testRelayOptions(t *testing.T, cert tls.Certificate) RelayOptions {
	t.Helper()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	identity := config.CertificateInfo{
		PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})),
		PemPrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})),
	}
	sign, err := cloudrelay.LitePrincipalSigner(identity.PemCertificate, []byte(identity.PemPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	return RelayOptions{OperatorIdentity: identity, SignRequest: sign}
}

// Use the shared, text-reviewable vector without embedding opaque binary patches.
func relayGrantFixture() ([]byte, error) {
	data, err := os.ReadFile("../../../../Proto/conformance/lite-relay/vector.json")
	if err != nil {
		return nil, err
	}
	var vector struct {
		Record string `json:"record_hex"`
	}
	if err := json.Unmarshal(data, &vector); err != nil {
		return nil, err
	}
	return hex.DecodeString(vector.Record)
}
