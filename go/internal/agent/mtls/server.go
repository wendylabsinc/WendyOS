// Package mtls provides helpers for creating gRPC servers with mutual TLS authentication.
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
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

	cert, err := certs.TLSKeyPair(certPEM, chainPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("loading X509 key pair: %w", err)
	}

	caPool := x509.NewCertPool()
	certs.AppendChainToPool(caPool, chainPEM)
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
	// Direct PKI device certificates carry a tenant SPIFFE identity instead
	// of a legacy numeric org. Keep tenant enforcement active even when the
	// legacy org interceptor has no numeric identity to compare. VerifyConnection
	// runs for resumed sessions as well as full certificate handshakes.
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parsing device certificate: %w", err)
	}
	if own, ok := certs.TenantPrincipalFromCert(leaf); ok {
		parts := strings.Split(own, "/")
		if len(parts) >= 7 && parts[5] == "device" {
			tenantPrefix := strings.Join(parts[:5], "/") + "/"
			previous := cfg.VerifyConnection
			cfg.VerifyConnection = func(cs tls.ConnectionState) error {
				if previous != nil {
					if err := previous(cs); err != nil {
						return err
					}
				}
				if len(cs.PeerCertificates) == 0 {
					return fmt.Errorf("client certificate is required")
				}
				peer, ok := certs.TenantPrincipalFromCert(cs.PeerCertificates[0])
				if !ok || !strings.HasPrefix(peer, tenantPrefix) {
					return fmt.Errorf("client certificate does not belong to the device's PKI tenant")
				}
				return nil
			}
		}
	}
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

	known, err := parseCertsFromPEM([]byte(chainPEM))
	if err != nil {
		return nil, fmt.Errorf("parsing agent issuer material: %w", err)
	}
	tlsConfig.NextProtos = []string{"h2"}
	meshalpn.NewCertificates(tlsConfig.Certificates[0], known).ConfigureServer(tlsConfig)

	creds := meshoperatorlink.AgentTransportCredentials(credentials.NewTLS(tlsConfig))
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
			Timeout: 20 * time.Second,
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

// NewClientTLSConfigExpectingDevice is like NewClientTLSConfig but additionally
// pins the expected peer identity, closing the mesh LAN-dial spoofing gap:
// mDNS advertises a device's SPIFFE principal over an unauthenticated TXT
// record (discoverOnLAN), so chain validity alone — what NewClientTLSConfig
// checks — only proves the peer holds a cert signed by a trusted CA, not that
// it is the specific device the caller intended to reach. Any other
// same-CA-issued cert could otherwise impersonate the mDNS-advertised target
// and MITM the connection. The returned config's VerifyPeerCertificate runs
// the normal chain check first, then requires the peer leaf to carry a tenant
// SPIFFE device principal equal to want.
//
// The caller's tenant is read off certPEM rather than passed in. The
// invariant is "a device in the tenant I am in", so a
// want naming a foreign tenant is rejected even if some CA-signed leaf
// somewhere matches it. A caller without a tenant identity is refused;
// a valid operator credential can establish the same tenant boundary.
func NewClientTLSConfigExpectingDevice(certPEM, chainPEM, keyPEM string, logger *zap.Logger, want meshid.Device) (*tls.Config, error) {
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
		if !haveOwnScope || ownScope.TenantUUID == "" {
			return errors.New("mtls: caller has no tenant identity")
		}
		if want.Tenant().String() != ownScope.TenantUUID {
			return fmt.Errorf("mtls: expected peer %s outside own tenant %s", want.Principal(), ownScope.TenantUUID)
		}
		if err := VerifyPeerDevice(leaf, ownScope.TenantUUID, want.UUID().String()); err != nil {
			return err
		}
		return nil
	}
	applyPeerPin(base, pinnedVerify)
	return base, nil
}

// NewClientTLSConfigExpectingIdentity is the non-mesh sibling of
// NewClientTLSConfigExpectingDevice for dials whose identity contract is a
// certs.WendyIdentity rather than a mesh device principal — sensor pairing
// reaches source agents and Wendy Lite boards, which may still carry legacy
// urn:wendy asset identities or transitional dual-SAN leaves. The peer leaf
// must name exactly `want` (SameEntity: principals compared when both sides
// have one, legacy scope+type+id otherwise) and live in this device's own
// tenant when that tenant is knowable.
func NewClientTLSConfigExpectingIdentity(certPEM, chainPEM, keyPEM string, logger *zap.Logger, want certs.WendyIdentity) (*tls.Config, error) {
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
		peer, found, err := certs.IdentityFromCert(leaf)
		if err != nil {
			return fmt.Errorf("mtls: parsing peer wendy identity: %w", err)
		}
		if !found {
			return errors.New("mtls: peer certificate carries no wendy identity")
		}
		if !peer.SameEntity(want) {
			return fmt.Errorf("mtls: expected peer %s, got %s (%s)", want.IdentityKey(), peer.IdentityKey(), peer.Scope())
		}
		if haveOwnScope && !peer.Scope().Matches(ownScope) {
			return fmt.Errorf("mtls: peer %s is outside own tenant %s", peer.IdentityKey(), ownScope)
		}
		return nil
	}
	applyPeerPin(base, pinnedVerify)
	return base, nil
}

// applyPeerPin installs pinnedVerify on both handshake paths of a client
// config. CRITICAL: a resumed TLS 1.3 handshake skips VerifyPeerCertificate
// entirely — Go only calls VerifyConnection, with ConnectionState.
// PeerCertificates restored from the cached session ticket. All mesh dials
// share one process-wide meshSessionCache AND one constant gRPC authority
// (passthrough:///mesh-peer), so without the VerifyConnection hook a ticket
// cached while dialing device A could be resumed while "expecting" device B,
// silently bypassing the anti-MITM pin (see meshDialLAN's doc in
// mesh_dialer.go). The hook re-runs the SAME pinned verify closure against
// the resumed connection's restored peer certs; on a full handshake
// VerifyPeerCertificate already ran it, so there is nothing to redo. A pin
// mismatch hard-fails rather than silently declining to resume: a full
// handshake to the wrong peer would fail this same pin check, so there is no
// decline-vs-fail asymmetry to preserve.
//
// With this in place, every resumed connection re-verifies the full chain +
// identity pin, so a client that let session tickets chain indefinitely
// across resumptions (extending trust well past a single handshake's ML-DSA
// verification — see tlscache.Cache.SetResumed's doc for why the CLI↔agent
// path guards against exactly that) is harmless here: the in-memory
// meshSessionCache needs no equivalent guard.
func applyPeerPin(base *tls.Config, pinnedVerify func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error) {
	base.VerifyPeerCertificate = pinnedVerify
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
}

// VerifyPeerDevice is the device-to-device rule, applied after the chain check
// (tenant Device CA or its Lite sibling, up to the platform Root): the peer leaf
// carries exactly one tenant SPIFFE device principal, its tenant is ownTenant,
// and its device UUID is wantDevice, the roster entry for the dialed name. The
// address the dial went to is only a routing hint and proves nothing.
func VerifyPeerDevice(leaf *x509.Certificate, ownTenant, wantDevice string) error {
	if ownTenant == "" {
		return fmt.Errorf("mtls: expected peer device %s, but this device has no tenant identity", wantDevice)
	}
	peer, err := meshid.DeviceFromCert(leaf)
	if err != nil {
		return fmt.Errorf("mtls: expected peer device %s: %w", wantDevice, err)
	}
	if peer.Tenant().String() != ownTenant || peer.UUID().String() != wantDevice {
		return fmt.Errorf("mtls: expected peer device %s in tenant %s, got %s", wantDevice, ownTenant, peer.Principal())
	}
	return nil
}
