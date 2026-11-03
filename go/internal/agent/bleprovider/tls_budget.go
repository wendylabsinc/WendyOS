package bleprovider

import (
	"context"
	"crypto/tls"
	"time"
)

const (
	// A busy adapter may still be authenticating another neighbour. Let its
	// bounded accepted-CoC queue wait for that certificate flight instead of
	// repeatedly closing and recreating radio connections. Link-layer
	// supervision is independent of this application admission wait.
	meshTLSAdmissionWait = 30 * time.Second
	// Once admitted, allow an mTLS certificate flight to progress over a busy
	// multi-ACL adapter. The CoC's write-idle and ACL supervision limits remain
	// independent bounds when the controller stops making progress.
	meshTLSHandshakeTimeout = 30 * time.Second
	// A client cannot observe remote admission. Its total budget covers that
	// wait followed by the peer's bounded authentication flight. Runtime or
	// caller cancellation still interrupts either phase immediately.
	meshTLSOutboundTimeout = meshTLSAdmissionWait + meshTLSHandshakeTimeout
)

func handshakeMeshOutbound(ctx context.Context, secure *tls.Conn) (time.Duration, error) {
	handshakeCtx, cancel := context.WithTimeout(ctx, meshTLSOutboundTimeout)
	defer cancel()
	started := time.Now()
	err := secure.HandshakeContext(handshakeCtx)
	return time.Since(started), err
}
