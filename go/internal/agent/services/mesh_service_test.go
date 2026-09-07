package services

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func certWithURN(t *testing.T, urn string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(urn)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		URIs:         []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func ctxWithPeerCert(cert *x509.Certificate) context.Context {
	return ctxWithPeerCertFrom(context.Background(), cert)
}

func ctxWithPeerCertFrom(parent context.Context, cert *x509.Certificate) context.Context {
	return peer.NewContext(parent, &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}},
	})
}

// fakeMeshDialStream implements agentpbv2.WendyMeshService_MeshDialServer.
type fakeMeshDialStream struct {
	agentpbv2.WendyMeshService_MeshDialServer // panics if an unstubbed method is hit
	ctx                                       context.Context
	in                                        chan *agentpbv2.MeshDialMessage
	out                                       chan *agentpbv2.MeshDialData
}

func (f *fakeMeshDialStream) Context() context.Context { return f.ctx }
func (f *fakeMeshDialStream) Recv() (*agentpbv2.MeshDialMessage, error) {
	m, ok := <-f.in
	if !ok {
		return nil, io.EOF
	}
	return m, nil
}
func (f *fakeMeshDialStream) Send(d *agentpbv2.MeshDialData) error {
	f.out <- d
	return nil
}

// ctxAwareMeshDialStream mimics a real gRPC server stream when the client's
// stream/connection dies: Recv and Send unblock with the context error as
// soon as the stream context is cancelled. No half-close frame is ever
// delivered — cancellation is the only termination signal, modeling an
// abrupt client disappearance.
type ctxAwareMeshDialStream struct {
	agentpbv2.WendyMeshService_MeshDialServer // panics if an unstubbed method is hit
	ctx                                       context.Context
	in                                        chan *agentpbv2.MeshDialMessage
	out                                       chan *agentpbv2.MeshDialData
}

func (f *ctxAwareMeshDialStream) Context() context.Context { return f.ctx }
func (f *ctxAwareMeshDialStream) Recv() (*agentpbv2.MeshDialMessage, error) {
	select {
	case m, ok := <-f.in:
		if !ok {
			return nil, io.EOF
		}
		return m, nil
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	}
}
func (f *ctxAwareMeshDialStream) Send(d *agentpbv2.MeshDialData) error {
	select {
	case f.out <- d:
		return nil
	case <-f.ctx.Done():
		return f.ctx.Err()
	}
}

// testScope is the device's own tenant used by newMeshServiceForTest; every
// existing fixture cert in this file carries "urn:wendy:org:7:...", so this
// must stay org 7 for the pre-existing tests to keep passing under the
// same-tenant check.
var testScope = certs.Scope{OrgID: 7}

func newMeshServiceForTest(t *testing.T) (*MeshService, string) {
	t.Helper()
	dir := t.TempDir()
	return NewMeshService(zap.NewNop(), dir, testScope), dir
}

func TestMeshDialRejectsUserCert(t *testing.T) {
	svc, _ := newMeshServiceForTest(t)
	stream := &fakeMeshDialStream{ctx: ctxWithPeerCert(certWithURN(t, "urn:wendy:org:7:user:9"))}
	err := svc.MeshDial(stream)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err = %v, want PermissionDenied", err)
	}
}

// TestMeshDialRejectsCrossOrgAsset covers the gap the mTLS org interceptor
// leaves open when disabled (WENDY_MTLS_ORG_ENFORCEMENT=off, C-final-review
// Fix 4): MeshService must independently enforce same-org, not delegate
// solely to the interceptor. A same-CA asset certificate from a different org
// than this device's own must be rejected even though it is a valid,
// non-expired asset certificate.
func TestMeshDialRejectsCrossOrgAsset(t *testing.T) {
	svc, _ := newMeshServiceForTest(t)
	stream := &fakeMeshDialStream{ctx: ctxWithPeerCert(certWithURN(t, "urn:wendy:org:9:asset:215"))}
	if status.Code(svc.MeshDial(stream)) != codes.PermissionDenied {
		t.Fatal("want PermissionDenied for an asset certificate from a different org")
	}
}

// TestMeshDialSkipsOrgCheckWhenDeviceOrgUnknown mirrors the mTLS
// interceptor's grace behavior: main.go forces org enforcement off when this
// device could not determine its own tenant from its certificate (an unknown
// Scope), because there is nothing meaningful to compare against. MeshService
// must skip its own-tenant check in that case rather than reject every caller —
// the request must still fail on other grounds (port 0) to prove the org gate
// was skipped, not that the whole call short-circuited to success.
func TestMeshDialSkipsOrgCheckWhenDeviceOrgUnknown(t *testing.T) {
	dir := t.TempDir()
	svc := NewMeshService(zap.NewNop(), dir, certs.Scope{})
	in := make(chan *agentpbv2.MeshDialMessage, 1)
	in <- &agentpbv2.MeshDialMessage{Content: &agentpbv2.MeshDialMessage_Open{Open: &agentpbv2.MeshDialOpen{Port: 0}}}
	stream := &fakeMeshDialStream{ctx: ctxWithPeerCert(certWithURN(t, "urn:wendy:org:42:asset:215")), in: in}
	if status.Code(svc.MeshDial(stream)) != codes.InvalidArgument {
		t.Fatal("want InvalidArgument for port 0 (org check must be skipped, not rejected, when device org is unknown)")
	}
}

func TestMeshDialRejectsNoPeer(t *testing.T) {
	svc, _ := newMeshServiceForTest(t)
	stream := &fakeMeshDialStream{ctx: context.Background()}
	if status.Code(svc.MeshDial(stream)) != codes.PermissionDenied {
		t.Fatal("want PermissionDenied without mTLS peer info")
	}
}

func TestMeshDialRejectsWhenDisabled(t *testing.T) {
	svc, dir := newMeshServiceForTest(t)
	if err := os.WriteFile(filepath.Join(dir, "mesh-disabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stream := &fakeMeshDialStream{ctx: ctxWithPeerCert(certWithURN(t, "urn:wendy:org:7:asset:215"))}
	if status.Code(svc.MeshDial(stream)) != codes.PermissionDenied {
		t.Fatal("want PermissionDenied when mesh-disabled file exists")
	}
}

func TestMeshDialRequiresOpenFirst(t *testing.T) {
	svc, _ := newMeshServiceForTest(t)
	in := make(chan *agentpbv2.MeshDialMessage, 1)
	in <- &agentpbv2.MeshDialMessage{Content: &agentpbv2.MeshDialMessage_Data{Data: &agentpbv2.MeshDialData{Payload: []byte("x")}}}
	stream := &fakeMeshDialStream{ctx: ctxWithPeerCert(certWithURN(t, "urn:wendy:org:7:asset:215")), in: in}
	if status.Code(svc.MeshDial(stream)) != codes.InvalidArgument {
		t.Fatal("want InvalidArgument when first message is not open")
	}
}

func TestMeshDialRejectsPortZero(t *testing.T) {
	svc, _ := newMeshServiceForTest(t)
	in := make(chan *agentpbv2.MeshDialMessage, 1)
	in <- &agentpbv2.MeshDialMessage{Content: &agentpbv2.MeshDialMessage_Open{Open: &agentpbv2.MeshDialOpen{Port: 0}}}
	stream := &fakeMeshDialStream{ctx: ctxWithPeerCert(certWithURN(t, "urn:wendy:org:7:asset:215")), in: in}
	if status.Code(svc.MeshDial(stream)) != codes.InvalidArgument {
		t.Fatal("want InvalidArgument for port 0")
	}
}

// TestMeshDialUnblocksWhenClientStreamDies covers the abrupt-disconnect path:
// the local service accepts but never writes or closes, so the local→stream
// goroutine sits in a blocked conn.Read. When the client's stream dies (gRPC
// cancels the stream context), MeshDial must still return instead of leaking
// the handler, the goroutine, and the local socket.
func TestMeshDialUnblocksWhenClientStreamDies(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c // hold the conn open; never write, never close
	}()

	svc, _ := newMeshServiceForTest(t)
	svc.dialLocal = func(_ string, timeout time.Duration) (net.Conn, error) {
		return net.DialTimeout("tcp", ln.Addr().String(), timeout)
	}

	streamCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan *agentpbv2.MeshDialMessage, 1)
	in <- &agentpbv2.MeshDialMessage{Content: &agentpbv2.MeshDialMessage_Open{Open: &agentpbv2.MeshDialOpen{Port: 8080}}}
	stream := &ctxAwareMeshDialStream{
		ctx: ctxWithPeerCertFrom(streamCtx, certWithURN(t, "urn:wendy:org:7:asset:215")),
		in:  in,
		out: make(chan *agentpbv2.MeshDialData, 4),
	}

	done := make(chan error, 1)
	go func() { done <- svc.MeshDial(stream) }()

	// Wait until the relay has dialed the local service, so the local→stream
	// goroutine is (about to be) parked in conn.Read before we kill the stream.
	select {
	case c := <-accepted:
		defer c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("MeshDial never dialed the local service")
	}

	cancel() // abrupt client death: gRPC cancels the stream context

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("MeshDial did not return after the client stream died")
	}
}

func TestMeshDialRelayHardWriteFailureUnblocksOppositeDirection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan *agentpbv2.MeshDialMessage, 1)
	in <- &agentpbv2.MeshDialMessage{Content: &agentpbv2.MeshDialMessage_Data{
		Data: &agentpbv2.MeshDialData{Payload: []byte("upload bytes")},
	}}
	stream := &ctxAwareMeshDialStream{
		ctx: ctx,
		in:  in,
		out: make(chan *agentpbv2.MeshDialData, 1),
	}

	relayPipe, blockedPeer := net.Pipe()
	defer blockedPeer.Close()
	conn := &failWriteConn{Conn: relayPipe, err: errors.New("registry reset")}

	svc, _ := newMeshServiceForTest(t)
	done := make(chan error, 1)
	go func() { done <- svc.relay(stream, conn) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relay returned %v, want graceful handler teardown", err)
		}
	case <-time.After(time.Second):
		t.Fatal("mesh relay remained blocked after the registry-side write failed")
	}
}

func TestMeshDialRelaysToLocalPort(t *testing.T) {
	// Local echo server standing in for a published app port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 64)
		n, _ := c.Read(buf)
		fmt.Fprintf(c, "echo:%s", buf[:n])
		c.Close()
	}()

	svc, _ := newMeshServiceForTest(t)
	// Point local dialing at the test listener regardless of requested port.
	svc.dialLocal = func(_ string, timeout time.Duration) (net.Conn, error) {
		return net.DialTimeout("tcp", ln.Addr().String(), timeout)
	}

	in := make(chan *agentpbv2.MeshDialMessage, 3)
	out := make(chan *agentpbv2.MeshDialData, 3)
	in <- &agentpbv2.MeshDialMessage{Content: &agentpbv2.MeshDialMessage_Open{Open: &agentpbv2.MeshDialOpen{Port: 8080}}}
	in <- &agentpbv2.MeshDialMessage{Content: &agentpbv2.MeshDialMessage_Data{Data: &agentpbv2.MeshDialData{Payload: []byte("ping")}}}
	close(in)
	stream := &fakeMeshDialStream{ctx: ctxWithPeerCert(certWithURN(t, "urn:wendy:org:7:asset:215")), in: in, out: out}

	if err := svc.MeshDial(stream); err != nil {
		t.Fatalf("MeshDial: %v", err)
	}
	var got []byte
	for d := range collectUntilHalfClose(out) {
		got = append(got, d...)
	}
	if string(got) != "echo:ping" {
		t.Fatalf("relayed %q, want %q", got, "echo:ping")
	}
}

// collectUntilHalfClose drains payloads until a half_close frame arrives.
func collectUntilHalfClose(out chan *agentpbv2.MeshDialData) <-chan []byte {
	ch := make(chan []byte)
	go func() {
		defer close(ch)
		for d := range out {
			if len(d.Payload) > 0 {
				ch <- d.Payload
			}
			if d.HalfClose {
				return
			}
		}
	}()
	return ch
}
