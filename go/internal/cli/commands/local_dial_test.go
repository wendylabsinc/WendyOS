package commands

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/cli/closedtest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/localkey"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

type localVersionAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
}

func (localVersionAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{Version: "test"}, nil
}

// closedDir is an agent config dir whose access file closes the device with
// owner as its first owner.
func closedDir(t *testing.T, owner crypto.Signer) string {
	t.Helper()
	dir := t.TempDir()
	keys, err := trustedkeys.Open(filepath.Join(dir, trustedkeys.FileName))
	if err != nil {
		t.Fatal(err)
	}
	spki, err := localkey.PublicKeyDER(owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Close(spki, "owner", time.Now()); err != nil {
		t.Fatal(err)
	}
	return dir
}

// startLocalOnlyAgent runs a Closed device (the agent's real key listener,
// role check and access service) with owner as its only key, and returns
// the address discovery advertises: the plaintext port.
func startLocalOnlyAgent(t *testing.T, owner crypto.Signer) string {
	t.Helper()
	return closedtest.Start(t, closedDir(t, owner)).Addr
}

// startEnrolledAgent serves a certificate naming a tenant identity, as a
// cloud-enrolled device does.
func startEnrolledAgent(t *testing.T) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	u, _ := url.Parse("urn:wendy:org:7:asset:9")
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "enrolled"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		URIs:         []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		ClientAuth:   tls.RequireAnyClientCert,
	})))
	agentpb.RegisterWendyAgentServiceServer(srv, localVersionAgent{})
	return serveTest(t, srv)
}

func serveTest(t *testing.T, srv *grpc.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

// useLocalKey installs this computer's local operator key for the test.
func useLocalKey(t *testing.T) crypto.Signer {
	t.Helper()
	key, err := localkey.LoadOrCreate(filepath.Join(t.TempDir(), localkey.FileName))
	if err != nil {
		t.Fatal(err)
	}
	orig := localOperatorKeyFn
	localOperatorKeyFn = func() (crypto.Signer, error) { return key, nil }
	t.Cleanup(func() { localOperatorKeyFn = orig })
	return key
}

func forbidPlaintext(t *testing.T) {
	t.Helper()
	orig := plaintextConnectFn
	plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Error("ladder fell back to plaintext")
		return nil, errors.New("plaintext forbidden")
	}
	t.Cleanup(func() { plaintextConnectFn = orig })
}

func ladderCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestLadderDialsLocalOnlyDeviceWithLocalKey(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	forbidPlaintext(t)
	owner := useLocalKey(t)
	addr := startLocalOnlyAgent(t, owner)

	// With and without cloud certificates: the cloud rung fails against the
	// device's self-signed certificate and the local key carries the dial.
	for _, certs := range [][]config.CertificateInfo{nil, {selfSignedCLICert(t, 7)}} {
		conn, _, err := dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", addr), certs)
		if err != nil {
			t.Fatalf("certs=%d: ladder: %v", len(certs), err)
		}
		if !conn.IsLocalOperator || !conn.IsMTLS {
			t.Fatalf("certs=%d: connection is not the local-operator one", len(certs))
		}
		conn.Close()
	}
	store, err := openLocalPinStoreFn()
	if err != nil {
		t.Fatal(err)
	}
	if !store.Has(devicepin.LocalPinKey("box")) {
		t.Fatal("device key not pinned on first contact")
	}
}

func TestLadderRefusesWhenLocalKeyNotTrusted(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	other, _ := localkey.LoadOrCreate(filepath.Join(t.TempDir(), localkey.FileName))
	addr := startLocalOnlyAgent(t, other)
	mine := useLocalKey(t)

	_, _, err := dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", addr), nil)
	if !errors.Is(err, errProvisionedAgentUnauthorized) {
		t.Fatalf("err = %v, want an unauthorized refusal", err)
	}
	fp, _ := localkey.Fingerprint(mine)
	for _, want := range []string{"is Closed", fp, "wendy device keys add", "wendy auth local-key show"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
}

// A Closed device whose owner opened the keyless apps set: a computer whose
// key it does not trust gets the plaintext connection, and only that set.
func TestLadderUsesKeylessSetWhenKeyNotTrusted(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	other, _ := localkey.LoadOrCreate(filepath.Join(t.TempDir(), localkey.FileName))
	dir := closedDir(t, other)
	keys, _ := trustedkeys.Open(filepath.Join(dir, trustedkeys.FileName))
	if err := keys.SetKeylessApps(true, keys.List()[0].Fingerprint()); err != nil {
		t.Fatal(err)
	}
	addr := closedtest.Start(t, dir).Addr
	useLocalKey(t)

	conn, _, err := dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", addr), nil)
	if err != nil {
		t.Fatalf("ladder: %v", err)
	}
	defer conn.Close()
	if conn.IsMTLS || conn.IsLocalOperator {
		t.Fatal("want the plaintext keyless connection")
	}
	if _, err := agentpbv2.NewWendyAccessServiceClient(conn.Conn).ListTrustedKeys(ladderCtx(t), &agentpbv2.ListTrustedKeysRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ListTrustedKeys without a key = %v, want PermissionDenied", err)
	}
}

// An Open device has no key port: the ladder uses plaintext and pins nothing.
func TestLadderOpenDeviceUsesPlaintext(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	useLocalKey(t)
	addr := closedtest.Start(t, t.TempDir()).Addr

	conn, _, err := dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", addr), nil)
	if err != nil {
		t.Fatalf("ladder: %v", err)
	}
	defer conn.Close()
	if conn.IsMTLS {
		t.Fatal("want plaintext for an Open device")
	}
	if store, _ := openLocalPinStoreFn(); store.Has(devicepin.LocalPinKey("box")) {
		t.Fatal("an Open device's name was pinned")
	}
}

// Enrolled: the cloud identity is used; the local rung never runs.
func TestLadderUsesCloudIdentityFirst(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	forbidPlaintext(t)
	cloudConn := grpcclient.NewFromConn(nil)
	stubMTLSCandidate(t, func(context.Context, *mtlsWalk, string, bool) (*grpcclient.AgentConnection, error) {
		return cloudConn, nil
	})
	orig := dialLocalOperatorFn
	dialLocalOperatorFn = func(context.Context, dialTarget, []string) (*grpcclient.AgentConnection, localDialResult, error) {
		t.Error("local rung ran although the cloud identity connected")
		return nil, localDialResult{}, nil
	}
	t.Cleanup(func() { dialLocalOperatorFn = orig })

	conn, _, err := dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", "127.0.0.1:50051"), []config.CertificateInfo{selfSignedCLICert(t, 7)})
	if err != nil || conn != cloudConn {
		t.Fatalf("ladder = (%v, %v), want the cloud connection", conn, err)
	}
}

func TestLadderEnrolledDeviceWithoutCloudLoginRefuses(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	forbidPlaintext(t)
	useLocalKey(t)
	addr := startEnrolledAgent(t)

	_, _, err := dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", addr), nil)
	if !errors.Is(err, errProvisionedAgentUnauthorized) || !strings.Contains(err.Error(), "wendy auth login") {
		t.Fatalf("err = %v, want a refusal naming wendy auth login", err)
	}
}

func TestLadderNeverReachesLocallyPinnedDeviceInTheClear(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	forbidPlaintext(t)
	owner := useLocalKey(t)
	addr := startLocalOnlyAgent(t, owner)
	conn, _, err := dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", addr), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()

	// Same name, nothing listening now.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	_, _, err = dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", dead), nil)
	if !errors.Is(err, errNoAuthenticatedEndpoint) {
		t.Fatalf("err = %v, want no-authenticated-endpoint refusal", err)
	}
}

func TestLadderRefusesChangedLocalDeviceKey(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	forbidPlaintext(t)
	owner := useLocalKey(t)
	conn, _, err := dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", startLocalOnlyAgent(t, owner)), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()

	_, _, err = dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", startLocalOnlyAgent(t, owner)), nil)
	if !errors.Is(err, errDeviceIdentityRefused) || !strings.Contains(err.Error(), "wendy device unpin box") {
		t.Fatalf("err = %v, want an identity refusal naming wendy device unpin", err)
	}
}

// A name pinned to a cloud-enrolled identity that now answers as a locally
// owned device is the identity change the pin exists to catch.
func TestLadderRefusesCloudPinnedHostAnsweringAsLocalDevice(t *testing.T) {
	owner := useLocalKey(t)
	addr := startLocalOnlyAgent(t, owner)
	host, _, _ := net.SplitHostPort(addr)
	setTempConfig(t, &config.Config{DevicePins: map[string]config.DevicePin{
		host: {OrgID: 7, CloudGRPC: "cloud.example:443", AssetID: "9"},
	}})
	setPinCache(t)
	forbidPlaintext(t)

	_, _, err := dialAgentLadderWithCerts(ladderCtx(t), newDialTarget(host, addr), nil)
	if !errors.Is(err, errDeviceIdentityRefused) {
		t.Fatalf("err = %v, want an identity refusal", err)
	}
}

// `wendy device unpin <name>`, the step the changed-key refusal names, clears
// the local device key pin.
func TestUnpinClearsLocalDeviceKeyPin(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	store, err := openLocalPinStoreFn()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := localkey.Certificate(useLocalKey(t), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CheckLocal([]string{"box"}, cert.Leaf); err != nil {
		t.Fatal(err)
	}
	cleared := clearPinsGoverning(&config.Config{}, "box")
	if len(cleared) != 1 || cleared[0].key != devicepin.LocalPinKey("box") {
		t.Fatalf("cleared = %+v", cleared)
	}
	if store, _ = openLocalPinStoreFn(); store.Has(devicepin.LocalPinKey("box")) {
		t.Fatal("local pin survived unpin")
	}
}

// Reviewer's scenario: a device that completes a verified local-operator
// handshake but then fails the version probe (here: no AgentService) must
// not be reached in plaintext on first contact.
func TestLadderNeverPlaintextAfterVerifiedLocalHandshake(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	forbidPlaintext(t)
	owner := useLocalKey(t)
	dir := closedDir(t, owner)
	keys, _ := trustedkeys.Open(filepath.Join(dir, trustedkeys.FileName))
	certPEM, keyPEM, err := mtls.ClosedServerIdentity(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := mtls.NewTrustedKeyTLSConfig(certPEM, keyPEM, keys, trustedkeys.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveTest(t, grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))) // no AgentService registered

	_, _, err = dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", addr), nil)
	if !errors.Is(err, errNoAuthenticatedEndpoint) || !strings.Contains(err.Error(), "wendy device update") {
		t.Fatalf("err = %v, want a refusal naming the agent update", err)
	}
}

// An unusable local key (group-readable, or sudo before one exists) is named
// in the refusal instead of being swallowed.
func TestLadderNamesUnusableLocalKey(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	forbidPlaintext(t)
	owner := useLocalKey(t)
	conn, _, err := dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", startLocalOnlyAgent(t, owner)), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	localOperatorKeyFn = func() (crypto.Signer, error) {
		return nil, errors.New("key.pem is accessible by group or others; run chmod 600 on it")
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	_, _, err = dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", dead), nil)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("err = %v, want the key's own error", err)
	}
}

// Closed-device pins are per name dialled: an alias from the discovery cache
// (unauthenticated mDNS) neither refuses another name nor names its pin.
func TestLadderIgnoresLocalPinUnderAliases(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t, discoverycache.Entry{ID: "dev-1", DisplayName: "box-alias", Hostname: "box.local"})
	forbidPlaintext(t)
	owner := useLocalKey(t)
	conn, _, err := dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box-alias", startLocalOnlyAgent(t, owner)), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()

	conn, _, err = dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box.local", startLocalOnlyAgent(t, owner)), nil)
	if err != nil {
		t.Fatalf("another device key under a different name was refused through an alias: %v", err)
	}
	conn.Close()
}

// A name pinned to a cloud identity whose host now answers as a Closed device
// that refuses this computer's key is refused, never reached in plaintext.
func TestLadderRefusesCloudPinnedHostRefusingLocalKey(t *testing.T) {
	other, _ := localkey.LoadOrCreate(filepath.Join(t.TempDir(), localkey.FileName))
	addr := startLocalOnlyAgent(t, other)
	host, _, _ := net.SplitHostPort(addr)
	setTempConfig(t, &config.Config{DevicePins: map[string]config.DevicePin{
		host: {OrgID: 7, CloudGRPC: "cloud.example:443", AssetID: "9"},
	}})
	setPinCache(t)
	forbidPlaintext(t)
	useLocalKey(t)

	_, _, err := dialAgentLadderWithCerts(ladderCtx(t), newDialTarget(host, addr), nil)
	if !errors.Is(err, errDeviceIdentityRefused) {
		t.Fatalf("err = %v, want an identity refusal", err)
	}
}

// A local-only device that completes its verified handshake and then closes
// the connection without an alert is refusing the key.
func TestLadderReadsCloseAfterVerifiedLocalHandshakeAsKeyRefused(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	useLocalKey(t)
	certPEM, keyPEM, err := mtls.ClosedServerIdentity(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := certs.TLSKeyPair(certPEM, "", keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = tls.Server(c, &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{"h2"}}).Handshake()
				c.Close() // no alert: just gone
			}()
		}
	}()

	// A refused key leaves only the keyless set, which is plaintext.
	errPlaintext := errors.New("plaintext rung reached")
	orig := plaintextConnectFn
	plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) { return nil, errPlaintext }
	t.Cleanup(func() { plaintextConnectFn = orig })

	_, _, err = dialAgentLadderWithCerts(ladderCtx(t), newDialTarget("box", ln.Addr().String()), nil)
	if !errors.Is(err, errPlaintext) {
		t.Fatalf("err = %v, want the key-refused reading (keyless plaintext rung)", err)
	}
}
