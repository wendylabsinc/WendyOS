//go:build !windows

package grpcclient_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/cli/closedtest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/localkey"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// localDevice runs a Closed device (the agent's real key listener, role
// check and access service) with owner as its only key, and returns its key
// port.
func localDevice(t *testing.T, owner crypto.Signer) string {
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
	host, port, _ := net.SplitHostPort(closedtest.Start(t, dir).Addr)
	p, _ := strconv.Atoi(port)
	return net.JoinHostPort(host, strconv.Itoa(p+1))
}

func newLocalKey(t *testing.T) crypto.Signer {
	t.Helper()
	key, err := localkey.LoadOrCreate(filepath.Join(t.TempDir(), localkey.FileName))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func probe(conn *grpcclient.AgentConnection) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	return err
}

func TestLocalKeyCertificateShape(t *testing.T) {
	key := newLocalKey(t)
	now := time.Now()
	cert, err := localkey.Certificate(key, now)
	if err != nil {
		t.Fatal(err)
	}
	leaf := cert.Leaf
	if leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) != nil {
		t.Error("certificate is not self-signed")
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		t.Error("certificate lacks the clientAuth EKU")
	}
	if _, found, err := certs.IdentityFromCert(leaf); found || err != nil {
		t.Errorf("certificate names a tenant identity (found=%v err=%v)", found, err)
	}
	if !leaf.NotBefore.Before(now) || leaf.NotAfter.Sub(leaf.NotBefore) > 48*time.Hour {
		t.Errorf("validity %v..%v, want a short window around now", leaf.NotBefore, leaf.NotAfter)
	}
	if fp, _ := localkey.Fingerprint(key); fp != trustedkeys.Fingerprint(leaf.RawSubjectPublicKeyInfo) {
		t.Error("certificate key is not the local key")
	}
}

func TestConnectWithLocalKeyReachesALocalDevice(t *testing.T) {
	owner := newLocalKey(t)
	addr := localDevice(t, owner)
	pins, err := devicepin.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	check := func(leaf *x509.Certificate) error { return pins.CheckLocal([]string{"box"}, leaf) }

	conn, err := grpcclient.ConnectWithLocalKey(context.Background(), addr, owner, check)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := probe(conn); err != nil {
		t.Fatalf("GetAgentVersion: %v", err)
	}
	if !conn.IsMTLS || !conn.IsLocalOperator || conn.CertInfo != nil {
		t.Fatalf("connection flags: mTLS=%v local=%v certInfo=%v", conn.IsMTLS, conn.IsLocalOperator, conn.CertInfo)
	}
	// The owner may manage keys over the same connection.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	list, err := agentpbv2.NewWendyAccessServiceClient(conn.Conn).ListTrustedKeys(ctx, &agentpbv2.ListTrustedKeysRequest{})
	if err != nil || len(list.GetKeys()) != 1 {
		t.Fatalf("ListTrustedKeys = %v, %v", list, err)
	}
	if !pins.Has(devicepin.LocalPinKey("box")) {
		t.Fatal("device key was not pinned on first contact")
	}
}

func TestConnectWithLocalKeyRefusedForUntrustedKey(t *testing.T) {
	addr := localDevice(t, newLocalKey(t))
	conn, err := grpcclient.ConnectWithLocalKey(context.Background(), addr, newLocalKey(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := probe(conn); err == nil {
		t.Fatal("device accepted a key it does not trust")
	}
	if conn.NotLocalDevice() {
		t.Fatal("an untrusted key was reported as a cloud-enrolled device")
	}
}

func TestConnectWithLocalKeyRefusesAChangedDeviceKey(t *testing.T) {
	owner := newLocalKey(t)
	pins, err := devicepin.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	check := func(leaf *x509.Certificate) error { return pins.CheckLocal([]string{"box"}, leaf) }
	first := localDevice(t, owner)
	conn, err := grpcclient.ConnectWithLocalKey(context.Background(), first, owner, check)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe(conn); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	// Same name, different device key: a reinstall or an impostor.
	second := localDevice(t, owner)
	conn, err = grpcclient.ConnectWithLocalKey(context.Background(), second, owner, check)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := probe(conn); err == nil {
		t.Fatal("connected to a device whose key differs from the pinned one")
	}
	if _, ok := conn.PinMismatch(); !ok {
		t.Fatal("pin mismatch not reported")
	}
}

// An enrolled device presents a CA-issued certificate naming its tenant; the
// local key is not offered to it as a credential.
func TestConnectWithLocalKeyRecognisesAnEnrolledDevice(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	u, _ := url.Parse("urn:wendy:org:5:asset:7")
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
	agentpb.RegisterWendyAgentServiceServer(srv, &versionService{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(srv.Stop)

	conn, err := grpcclient.ConnectWithLocalKey(context.Background(), ln.Addr().String(), newLocalKey(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := probe(conn); err == nil || !conn.NotLocalDevice() {
		t.Fatalf("probe err = %v, NotLocalDevice = %v; want refusal as cloud-enrolled", err, conn.NotLocalDevice())
	}
}

// A peer that replays a device's certificate without its key fails the
// handshake before the device is marked verified or its key pinned.
func TestConnectWithLocalKeyNeverTrustsAReplayedCertificate(t *testing.T) {
	devCert, devKey, err := mtls.ClosedServerIdentity(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	otherCert, otherKey, err := mtls.ClosedServerIdentity(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	device, err := certs.TLSKeyPair(devCert, "", devKey)
	if err != nil {
		t.Fatal(err)
	}
	other, err := certs.TLSKeyPair(otherCert, "", otherKey)
	if err != nil {
		t.Fatal(err)
	}
	replayed := tls.Certificate{Certificate: device.Certificate, PrivateKey: other.PrivateKey}
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
				_ = tls.Server(c, &tls.Config{Certificates: []tls.Certificate{replayed}, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{"h2"}}).Handshake()
				c.Close()
			}()
		}
	}()

	pinned := false
	conn, err := grpcclient.ConnectWithLocalKey(context.Background(), ln.Addr().String(), newLocalKey(t), func(*x509.Certificate) error {
		pinned = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if probe(conn) == nil {
		t.Fatal("probe succeeded against a replayed certificate")
	}
	if conn.LocalDeviceVerified() || pinned {
		t.Errorf("verified = %v, pinned = %v; want neither for a peer that did not prove the key", conn.LocalDeviceVerified(), pinned)
	}
}
