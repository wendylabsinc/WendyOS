package meshoperatorlink

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"
)

type retainedTestIdentity struct {
	state tls.ConnectionState
	done  chan struct{}
}

func (i retainedTestIdentity) TLSState() (tls.ConnectionState, error) { return i.state, nil }
func (i retainedTestIdentity) Done() <-chan struct{}                  { return i.done }
func retainedTestProof() retainedTestIdentity {
	return retainedTestIdentity{state: tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, PeerCertificates: []*x509.Certificate{{Raw: []byte{1, 2, 3}}}}, done: make(chan struct{})}
}

func TestRetainedAgentServerSetupAndLifetime(t *testing.T) {
	l := NewRetainedAgentListener()
	defer l.Close()
	identity := retainedTestProof()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	dialed := make(chan net.Conn, 1)
	failed := make(chan error, 1)
	go func() {
		c, err := l.Dial(ctx, identity)
		if err != nil {
			failed <- err
			return
		}
		dialed <- c
	}()
	server, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	var client net.Conn
	select {
	case client = <-dialed:
	case err := <-failed:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer client.Close()
	// The completed setup context does not own the admitted connection.
	cancel()
	raw, auth, err := AgentTransportCredentials(nil).ServerHandshake(server)
	if err != nil || raw != server {
		t.Fatalf("handoff: %v", err)
	}
	if info, ok := auth.(credentials.TLSInfo); !ok || info.State.PeerCertificates[0] != identity.state.PeerCertificates[0] {
		t.Fatal("lost admitted identity")
	}
	close(identity.done)
	client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("session end did not close pipe")
	}
}
