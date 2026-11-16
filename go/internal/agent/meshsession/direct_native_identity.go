package meshsession

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// DirectNativeIdentity is issued only by a retained session's authenticated
// admission. It pins that epoch's peer certificate, not the latest carrier's
// identity. Native consumers must preserve their ordinary service/RPC policy.
// The zero value is not authenticated. No exporter or TLS traffic secrets are
// retained by this identity snapshot.
type DirectNativeIdentity struct {
	peer  meshid.Device
	auth  *directNativeAuth
	check func() error
	done  <-chan struct{}
}

func (i DirectNativeIdentity) Peer() meshid.Device   { return i.peer }
func (i DirectNativeIdentity) Done() <-chan struct{} { return i.done }

// TLSState returns authentication metadata for an already secured stream.
// It is not a live TLS connection state and cannot export keying material.
func (i DirectNativeIdentity) TLSState() (tls.ConnectionState, error) {
	if i.auth == nil || i.check == nil {
		return tls.ConnectionState{}, ErrNoDirectSession
	}
	if err := i.check(); err != nil {
		return tls.ConnectionState{}, err
	}
	state := i.auth.state
	state.PeerCertificates = make([]*x509.Certificate, len(i.auth.certificates))
	for n, der := range i.auth.certificates {
		cert, err := x509.ParseCertificate(append([]byte(nil), der...))
		if err != nil {
			return tls.ConnectionState{}, err
		}
		state.PeerCertificates[n] = cert
	}
	return state, nil
}

type directNativeAuth struct {
	state        tls.ConnectionState
	certificates [][]byte
}

func snapshotDirectNativeAuth(state tls.ConnectionState) (*directNativeAuth, error) {
	if !state.HandshakeComplete || state.Version != tls.VersionTLS13 || len(state.PeerCertificates) == 0 {
		return nil, errors.New("native identity requires authenticated TLS 1.3 admission")
	}
	// Explicit fields avoid retaining TLS's private exporter closure and secrets.
	a := &directNativeAuth{state: tls.ConnectionState{Version: state.Version, HandshakeComplete: true, DidResume: state.DidResume, CipherSuite: state.CipherSuite, NegotiatedProtocol: state.NegotiatedProtocol}}
	for _, cert := range state.PeerCertificates {
		if cert == nil || len(cert.Raw) == 0 {
			return nil, errors.New("native identity missing certificate")
		}
		a.certificates = append(a.certificates, append([]byte(nil), cert.Raw...))
	}
	return a, nil
}

func (e *managedDirectSession) nativeIdentity(peer meshid.Device) DirectNativeIdentity {
	return DirectNativeIdentity{peer: peer, auth: e.nativeAuth, done: e.session.ctx.Done(), check: func() error {
		if err := e.session.ctx.Err(); err != nil {
			return err
		}
		deadline, err := e.lifetime()
		if err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return errors.New("retained native identity expired")
		}
		return nil
	}}
}
