package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type testRevocationStream struct{ ctx context.Context }

func (s testRevocationStream) Context() context.Context   { return s.ctx }
func (testRevocationStream) SetHeader(metadata.MD) error  { return nil }
func (testRevocationStream) SendHeader(metadata.MD) error { return nil }
func (testRevocationStream) SetTrailer(metadata.MD)       {}
func (testRevocationStream) SendMsg(any) error            { return nil }
func (testRevocationStream) RecvMsg(any) error            { return nil }

func TestRevocationUnaryAndIdleStream(t *testing.T) {
	ca, key, _ := testCAKeyPair(t)
	f := serveTestCRL(t, ca, key)
	leaf := revocationPeer(t, ca, key, f.server.URL)
	checker := newRevocationChecker([]*x509.Certificate{ca})
	checker.interval = time.Millisecond * 5
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}}})
	called := false
	_, err := checker.unaryInterceptor()(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { called = true; return nil, nil })
	if err != nil || !called {
		t.Fatalf("valid unary denied: %v", err)
	}
	started := make(chan struct{})
	canceled := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- checker.streamInterceptor()(nil, testRevocationStream{ctx}, &grpc.StreamServerInfo{}, func(_ any, stream grpc.ServerStream) error {
			close(started)
			<-stream.Context().Done()
			close(canceled)
			return stream.Context().Err()
		})
	}()
	<-started
	now := time.Now().Add(-time.Second)
	f.publish(t, ca, key, 2, now, now.Add(time.Hour), leaf.SerialNumber)
	select {
	case err := <-done:
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("idle revoked stream remained open")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("handler context not canceled")
	}
	called = false
	_, err = checker.unaryInterceptor()(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { called = true; return nil, nil })
	if status.Code(err) != codes.Unauthenticated || called {
		t.Fatalf("revoked unary reached handler: %v", err)
	}
}
