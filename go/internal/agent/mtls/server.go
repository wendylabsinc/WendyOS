// Package mtls provides helpers for creating gRPC servers with mutual TLS authentication.
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// NewTLSConfig creates a TLS config from PEM-encoded certificate, chain, and private key.
// The certificate and chain are concatenated to form the full server certificate chain.
// Client certificates are required and verified against the chain as a CA pool.
// ML-DSA (post-quantum) signed certificates are handled via a custom VerifyPeerCertificate
// callback because Go's crypto/x509 does not natively support ML-DSA signature verification.
// logger may be nil; when provided, rejected client certificates are logged at WARN level.
// notBeforeFloor is used as a lower bound on the current time for NotBefore checks so that
// certs remain valid when the device clock has not yet been synchronised via NTP. Pass a
// zero time.Time to disable the floor.
func NewTLSConfig(certPEM, chainPEM, keyPEM string, logger *zap.Logger, notBeforeFloor time.Time) (*tls.Config, error) {
	if chainPEM == "" {
		return nil, fmt.Errorf("CA chain PEM is required to verify client certificates; device may need to be re-provisioned")
	}

	// Only include the leaf cert in the TLS certificate — not the chain.
	// Go's TLS library calls x509.ParseCertificate on every cert sent in the
	// handshake, and ML-DSA chain certs (from pki-core) cause parse failures
	// on the receiving client. The chain is used below only for the CA pool.
	leafPEM, err := certs.LeafCertificatePEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("extracting leaf certificate: %w", err)
	}
	cert, err := tls.X509KeyPair([]byte(leafPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("loading X509 key pair: %w", err)
	}

	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM([]byte(chainPEM))
	caCerts, err := parseCertsFromPEM([]byte(chainPEM))
	if err != nil {
		return nil, fmt.Errorf("parsing chain PEM: %w", err)
	}
	if len(caCerts) == 0 {
		return nil, fmt.Errorf("parsing chain PEM: no certificates found")
	}
	caPool.AppendCertsFromPEM([]byte(certPEM))

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		// RequireAnyClientCert requires the client to present a cert but defers
		// chain verification to VerifyPeerCertificate, which handles ML-DSA.
		ClientAuth: tls.RequireAnyClientCert,
		// ClientCAs intentionally nil: AppendCertsFromPEM cannot parse ML-DSA
		// chain certs (trailing data), so the pool would only contain the leaf
		// cert's subject. Go's TLS client only sends its certificate when its
		// issuer appears in the server's AcceptableCAs list; with a mismatched
		// list it sends nothing and the handshake fails with "certificate required".
		// An empty ClientCAs list signals "accept any CA" — VerifyPeerCertificate
		// performs the actual ML-DSA-aware chain verification instead.
		ClientCAs:             nil,
		MinVersion:            tls.VersionTLS12,
		VerifyPeerCertificate: buildVerifyPeerCertificate(caPool, caCerts, logger, notBeforeFloor),
	}
	// Session resumption: stamp the client cert window into tickets and
	// decline stale ones (see session_ticket.go for the security rationale).
	wireSessionTicketChecks(cfg, notBeforeFloor, time.Now)
	return cfg, nil
}

// NewServer creates a gRPC server with mTLS credentials.
// The mTLS interceptors are always applied — they cannot be omitted via extraOpts.
// This ensures no handler can accidentally receive an unauthenticated call regardless
// of how the caller configures the server. Callers may add further interceptors via
// extraOpts; those run after the mandatory mTLS check.
// logger may be nil; when provided, rejected client certificates are logged at WARN level.
// notBeforeFloor is forwarded to NewTLSConfig; see its documentation for details.
// expected and orgMode are forwarded to the mandatory mTLS interceptors, which
// enforce tenant-equality between the connecting client cert and this device.
func NewServer(certPEM, chainPEM, keyPEM string, logger *zap.Logger, notBeforeFloor time.Time, expected certs.Scope, orgMode interceptor.OrgMode, extraOpts ...grpc.ServerOption) (*grpc.Server, error) {
	tlsConfig, err := NewTLSConfig(certPEM, chainPEM, keyPEM, logger, notBeforeFloor)
	if err != nil {
		return nil, fmt.Errorf("creating TLS config: %w", err)
	}

	creds := credentials.NewTLS(tlsConfig)
	opts := []grpc.ServerOption{
		grpc.Creds(creds),
		// mTLS interceptors are mandatory: they run before any caller-provided interceptors
		// so that no handler can be reached without a verified client certificate.
		grpc.ChainUnaryInterceptor(interceptor.UnaryMTLSInterceptor(logger, expected, orgMode)),
		grpc.ChainStreamInterceptor(interceptor.StreamMTLSInterceptor(logger, expected, orgMode)),
		grpc.InitialWindowSize(8 * 1024 * 1024),
		grpc.InitialConnWindowSize(16 * 1024 * 1024),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	}
	opts = append(opts, extraOpts...)
	return grpc.NewServer(opts...), nil
}

// meshSessionCache lets agent→agent mesh dials resume TLS sessions. One
// process-wide cache: mesh TLS configs are constructed per dial, so a
// per-config cache would never produce a hit, and the agent presents a single
// client identity so cross-identity ticket reuse cannot arise.
var meshSessionCache = tls.NewLRUClientSessionCache(64)

// NewClientTLSConfig returns a TLS config for one agent dialing another
// agent's mTLS port (mesh LAN path): it presents this device's asset
// certificate and verifies the peer's chain with the same custom verifier the
// server side uses (Go's built-in verification can't handle ML-DSA chains).
// Hostname verification is intentionally skipped — device certs carry wendy
// URN SANs, not DNS names.
func NewClientTLSConfig(certPEM, chainPEM, keyPEM string, logger *zap.Logger) (*tls.Config, error) {
	base, err := NewTLSConfig(certPEM, chainPEM, keyPEM, logger, time.Time{})
	if err != nil {
		return nil, err
	}
	if base.VerifyPeerCertificate == nil {
		// InsecureSkipVerify below is only safe because the custom verifier
		// replaces Go's built-in one; never hand out a config without it.
		return nil, errors.New("mtls: base TLS config has no peer verifier")
	}
	chainVerify := base.VerifyPeerCertificate
	cfg := &tls.Config{
		Certificates:          base.Certificates,
		MinVersion:            base.MinVersion,
		InsecureSkipVerify:    true, // verification is NOT disabled: VerifyPeerCertificate below performs the full (ML-DSA-aware) chain check
		VerifyPeerCertificate: chainVerify,
		ClientSessionCache:    meshSessionCache,
	}
	// Defense in depth: on a resumed TLS 1.3 handshake Go skips
	// VerifyPeerCertificate entirely (no certificate exchange happens) and
	// only calls VerifyConnection, with ConnectionState.PeerCertificates
	// restored from the cached session ticket rather than freshly presented.
	// Without re-running the chain check here, a resumed mesh connection
	// would perform zero client-side verification of the peer's certificate.
	// On a full handshake VerifyPeerCertificate already ran, so there is
	// nothing to redo.
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if !cs.DidResume {
			return nil
		}
		rawCerts := make([][]byte, len(cs.PeerCertificates))
		for i, c := range cs.PeerCertificates {
			rawCerts[i] = c.Raw
		}
		return chainVerify(rawCerts, nil)
	}
	return cfg, nil
}

// NewClientTLSConfigExpectingPeer is like NewClientTLSConfig but additionally
// pins the expected peer identity, closing the mesh LAN-dial spoofing gap:
// mDNS advertises a device's cloud asset ID over an unauthenticated TXT
// record (discoverOnLAN), so chain validity alone — what NewClientTLSConfig
// checks — only proves the peer holds a cert signed by a trusted CA, not that
// it is the specific device the caller intended to reach. Any other
// same-CA-issued cert (a different asset in the same org, or a user cert)
// could otherwise impersonate the mDNS-advertised target and MITM the
// connection. The returned config's VerifyPeerCertificate runs the normal
// chain check first, then requires the peer leaf to parse as a wendy asset
// identity with id wantAssetID, in this device's own tenant.
//
// The tenant is read off certPEM — this device's own leaf — rather than passed
// in: the invariant a mesh dial enforces is "the same tenant I am in", and both
// callers were already passing their own org. Deriving it here means a device
// whose certificate has moved from a urn:wendy org to a pki-core tenant SPIFFE
// principal compares like with like without either caller learning about it.
// A device that cannot determine its own tenant skips the tenant half of the
// check (the asset id still pins), mirroring the interceptor's grace behaviour
// rather than bricking mesh on an unidentifiable leaf.
func NewClientTLSConfigExpectingPeer(certPEM, chainPEM, keyPEM string, logger *zap.Logger, wantAssetID string) (*tls.Config, error) {
	base, err := NewClientTLSConfig(certPEM, chainPEM, keyPEM, logger)
	if err != nil {
		return nil, err
	}
	ownScope, haveOwnScope := certs.ScopeFromCertPEM(certPEM)
	chainVerify := base.VerifyPeerCertificate
	pinnedVerify := func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		if err := chainVerify(rawCerts, verifiedChains); err != nil {
			return err
		}
		if len(rawCerts) == 0 {
			return errors.New("mtls: no peer certificate presented")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("mtls: parsing peer certificate: %w", err)
		}
		ident, found, err := certs.IdentityFromCert(leaf)
		if err != nil {
			return fmt.Errorf("mtls: parsing peer wendy identity: %w", err)
		}
		if !found {
			return errors.New("mtls: peer certificate carries no wendy identity")
		}
		if ident.EntityType != certs.EntityAsset {
			return fmt.Errorf("mtls: expected peer asset %s in %s, got entity type %q", wantAssetID, ownScope, ident.EntityType)
		}
		// wantAssetID is the int32 cloud asset id the mesh addresses peers by
		// (mDNS TXT "assetid" → meshDialLAN). A device enrolled directly against
		// pki-core over ACME is named "device/<deviceID>" instead and has no
		// such id, so it cannot be reached over the mesh LAN path at all — the
		// gap is in mesh addressing, not here, and closing it means teaching
		// discovery to advertise the principal (WDY-2968 follow-up).
		if ident.EntityID != wantAssetID {
			return fmt.Errorf("mtls: expected peer asset %s in %s, got asset %s in %s",
				wantAssetID, ownScope, ident.EntityID, ident.Scope())
		}
		if haveOwnScope && !ident.Scope().Matches(ownScope) {
			return fmt.Errorf("mtls: expected peer asset %s in %s, got asset %s in %s",
				wantAssetID, ownScope, ident.EntityID, ident.Scope())
		}
		return nil
	}
	base.VerifyPeerCertificate = pinnedVerify
	// CRITICAL: a resumed TLS 1.3 handshake skips VerifyPeerCertificate
	// entirely — Go only calls VerifyConnection, with
	// ConnectionState.PeerCertificates restored from the cached session
	// ticket. All mesh dials share one process-wide meshSessionCache AND one
	// constant gRPC authority (passthrough:///mesh-peer), so without this,
	// a ticket cached while dialing asset A could be resumed while
	// "expecting" asset B, silently bypassing the anti-MITM pin above (see
	// meshDialLAN's doc in mesh_dialer.go). Re-run the SAME pinned verify
	// closure against the resumed connection's restored peer certs; on a
	// full handshake (DidResume false) VerifyPeerCertificate already ran
	// this check, so there is nothing to redo. A pin mismatch here hard-fails
	// the connection rather than silently declining to resume: a full
	// handshake to the wrong peer would fail this same pin check, so there
	// is no decline-vs-fail asymmetry to preserve.
	//
	// With this fix in place, every resumed mesh connection re-verifies the
	// full chain + identity pin, so a client that let session tickets chain
	// indefinitely across resumptions (extending trust well past a single
	// handshake's ML-DSA verification — see tlscache.Cache.SetResumed's doc
	// for why the CLI↔agent path guards against exactly that) is harmless
	// here: the in-memory meshSessionCache needs no equivalent guard.
	base.VerifyConnection = func(cs tls.ConnectionState) error {
		if !cs.DidResume {
			return nil
		}
		rawCerts := make([][]byte, len(cs.PeerCertificates))
		for i, c := range cs.PeerCertificates {
			rawCerts[i] = c.Raw
		}
		return pinnedVerify(rawCerts, nil)
	}
	return base, nil
}
