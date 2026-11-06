package grpcclient

import (
	"sync/atomic"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"google.golang.org/grpc"
)

// VerifiedServerIdentity carries a TLS verifier's accepted peer identity into a
// connection built outside this package. Its zero value has no trusted identity.
// Keep one observer per connection; never populate it from an expected identity
// or from an RPC response.
type VerifiedServerIdentity struct {
	identity atomic.Pointer[certs.WendyIdentity]
}

// Observe is an OnVerifiedServerIdentity callback for BuildServerVerifyConnection.
// It must only be called after the TLS verifier has accepted the peer's chain and
// expected identity. VerifyConnection also runs on resumed TLS handshakes.
func (v *VerifiedServerIdentity) Observe(identity certs.WendyIdentity) {
	if v != nil {
		verifiedIdentitySink(&v.identity)(identity)
	}
}

// NewFromConnWithVerifiedIdentity wraps a prebuilt connection and its TLS identity
// observer. Until a successful handshake calls observer.Observe, the connection
// reports no verified identity. A nil observer behaves like NewFromConn.
func NewFromConnWithVerifiedIdentity(conn *grpc.ClientConn, observer *VerifiedServerIdentity) *AgentConnection {
	ac := newAgentConnection(conn)
	if observer != nil {
		ac.observedServerIdentity = &observer.identity
	}
	return ac
}
