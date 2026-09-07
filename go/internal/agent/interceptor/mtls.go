package interceptor

import (
	"context"
	"crypto/x509"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// OrgMode selects how the mTLS gate enforces organization-equality between the
// connecting client certificate and the device's own organization.
type OrgMode int

const (
	// OrgModeOff disables the org check entirely; any validly-issued client cert
	// is accepted regardless of its organization.
	OrgModeOff OrgMode = iota
	// OrgModeGrace enforces org-equality for certs that carry an org identity but
	// allows legacy certs that carry no org identity (logging a warning), easing
	// migration before cert rotation completes.
	OrgModeGrace
	// OrgModeStrict enforces org-equality and additionally requires every client
	// cert to carry an org identity; legacy certs without one are rejected.
	OrgModeStrict
)

// String returns the canonical lowercase name of the mode for logging.
func (m OrgMode) String() string {
	switch m {
	case OrgModeOff:
		return "off"
	case OrgModeStrict:
		return "strict"
	case OrgModeGrace:
		return "grace"
	default:
		return "grace"
	}
}

// ParseOrgMode maps the WENDY_MTLS_ORG_ENFORCEMENT env value to a mode.
// An empty string yields (OrgModeStrict, true) — strict is the secure default:
// a client cert with no org identity is rejected. The values "grace", "strict",
// and "off" (case-insensitive, surrounding whitespace trimmed) yield the
// corresponding mode and true. Any other value yields (OrgModeStrict, false) so
// the caller can warn and still fail safe (reject no-org certs) rather than
// silently downgrading enforcement.
func ParseOrgMode(s string) (OrgMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return OrgModeStrict, true
	case "grace":
		return OrgModeGrace, true
	case "strict":
		return OrgModeStrict, true
	case "off":
		return OrgModeOff, true
	default:
		return OrgModeStrict, false
	}
}

func peerAddr(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return "unknown"
	}
	return p.Addr.String()
}

// CheckMTLS verifies that the gRPC context carries a mutually-authenticated TLS
// peer with at least one verified certificate chain. It logs and rejects calls
// that do not satisfy this requirement. Call this from RPC handlers that require
// explicit per-handler auth enforcement in addition to the server-level interceptor.
//
// Certificate revocation is handled at the TLS layer by the VerifyPeerCertificate
// hook in mtls.NewTLSConfig: it enforces a maximum certificate lifetime (2 years)
// as a compensating control, bounding exposure from a compromised credential.
// By the time this function runs the handshake has already applied that policy,
// so no duplicate revocation check is needed here.
//
// Audit logging: the certificate serial number (not PII) is logged at Debug level.
// Subject CN is intentionally omitted from per-call logs to satisfy data-minimisation
// requirements — it may contain a username or device identifier.
//
// Tenant enforcement: after the certificate is structurally validated, the
// caller's tenant scope (extracted from the leaf via certs.ScopeFromCert — the
// tenant SPIFFE principal pki-core stamps, or the legacy org URN on an old
// chain) is compared against expected under the given mode. This prevents a
// validly-issued user cert from one tenant being accepted by a device belonging
// to another (cross-tenant access). Only the scope (a tenant UUID or an org id,
// neither PII), the serial, and the remote address are logged — never the
// CN/subject, which may carry a username.
func CheckMTLS(ctx context.Context, logger *zap.Logger, expected certs.Scope, mode OrgMode) error {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		logger.Warn("rejected unauthenticated gRPC caller",
			zap.String("remote", peerAddr(ctx)))
		return status.Errorf(codes.Unauthenticated, "missing peer credentials")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		logger.Warn("rejected non-TLS gRPC caller",
			zap.String("remote", peerAddr(ctx)))
		return status.Errorf(codes.Unauthenticated, "mTLS authentication required")
	}
	// With RequireAnyClientCert + custom VerifyPeerCertificate, Go's TLS stack
	// never populates VerifiedChains (that requires VerifyClientCertIfGiven or
	// higher — see crypto/tls/handshake_server.go). PeerCertificates is always
	// populated when a client cert is presented, and the handshake's
	// VerifyPeerCertificate hook has already authenticated the ML-DSA chain
	// before this interceptor runs.
	if len(tlsInfo.State.PeerCertificates) == 0 {
		logger.Warn("rejected caller with no client certificate",
			zap.String("remote", peerAddr(ctx)))
		return status.Errorf(codes.Unauthenticated, "client certificate not verified")
	}
	leaf := tlsInfo.State.PeerCertificates[0]
	// Defence-in-depth EKU check: the leaf must explicitly assert clientAuth.
	// Certs with no EKU extension (empty ExtKeyUsage) are also rejected: absence
	// of the extension means the cert is technically unrestricted under RFC 5280,
	// but for a zero-trust mTLS service every client cert must be scoped. The TLS
	// handshake's VerifyPeerCertificate hook enforces this too, but re-checking here
	// ensures the constraint holds even if that hook is absent or the TLS config is
	// replaced without updating the server options.
	hasClientAuth := false
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageClientAuth {
			hasClientAuth = true
			break
		}
	}
	if !hasClientAuth {
		logger.Warn("rejected cert without clientAuth EKU",
			zap.String("remote", peerAddr(ctx)),
			zap.String("serial", leaf.SerialNumber.String()))
		return status.Errorf(codes.Unauthenticated, "certificate is not valid for client authentication")
	}
	// Log the serial number (not PII) at Debug level for per-call audit correlation.
	// Subject CN is omitted: it may contain a username or device identifier, logging
	// it on every call creates a high-volume PII stream that conflicts with
	// data-minimisation requirements.
	logger.Debug("mTLS peer authenticated",
		zap.String("remote", peerAddr(ctx)),
		zap.String("serial", leaf.SerialNumber.String()),
	)

	// Tenant-equality enforcement. OrgModeOff disables the check entirely,
	// preserving pre-WDY-1535 behaviour. For grace/strict we extract the cert's
	// scope and compare it to this device's own (expected).
	if mode == OrgModeOff {
		return nil
	}
	scope, known, err := certs.ScopeFromCert(leaf)
	switch {
	case err != nil:
		// An identity claim was present but malformed/ambiguous/non-positive.
		// This is anomalous; reject in BOTH grace and strict. The CN/subject is
		// not logged.
		logger.Warn("rejected cert with undeterminable tenant",
			zap.String("remote", peerAddr(ctx)),
			zap.String("serial", leaf.SerialNumber.String()),
			zap.Error(err))
		return status.Errorf(codes.PermissionDenied, "certificate tenant could not be determined")
	case known && scope.Matches(expected):
		return nil
	case known && scope.Comparable(expected):
		// A different tenant, said in terms this device can check. Refused in
		// grace as well as strict: grace forgives an identity it cannot read,
		// never one it can read and that says someone else.
		logger.Warn("rejected cert from non-permitted tenant",
			zap.String("remote", peerAddr(ctx)),
			zap.String("serial", leaf.SerialNumber.String()),
			zap.String("presented", scope.String()),
			zap.String("expected", expected.String()))
		return status.Errorf(codes.PermissionDenied, "certificate tenant not permitted")
	case known:
		// The caller named a tenant this device has no way to compare against
		// its own — a SPIFFE-only caller reaching a device still on an
		// old-chain certificate, or the reverse. There is no mapping between a
		// tenant UUID and an int32 org, so this is unprovable rather than
		// wrong, and it is exactly the state a fleet passes through while its
		// certificates rotate. Strict refuses it; grace is the knob for the
		// rotation window.
		if mode == OrgModeStrict {
			logger.Warn("rejected cert whose tenant cannot be compared with this device's under strict mode",
				zap.String("remote", peerAddr(ctx)),
				zap.String("serial", leaf.SerialNumber.String()),
				zap.String("presented", scope.String()),
				zap.String("expected", expected.String()))
			return status.Errorf(codes.PermissionDenied, "certificate tenant not permitted")
		}
		logger.Warn("client certificate names a tenant this device cannot compare with its own; allowed under grace mode (set WENDY_MTLS_ORG_ENFORCEMENT=strict once certificates have rotated)",
			zap.String("remote", peerAddr(ctx)),
			zap.String("serial", leaf.SerialNumber.String()),
			zap.String("presented", scope.String()),
			zap.String("expected", expected.String()))
		return nil
	default:
		// !known: a legacy cert with no identity at all.
		if mode == OrgModeStrict {
			logger.Warn("rejected cert with no tenant identity under strict mode",
				zap.String("remote", peerAddr(ctx)),
				zap.String("serial", leaf.SerialNumber.String()))
			return status.Errorf(codes.PermissionDenied, "certificate tenant identity required")
		}
		logger.Warn("client certificate has no tenant identity; allowed under grace mode (set WENDY_MTLS_ORG_ENFORCEMENT=strict after cert rotation)",
			zap.String("remote", peerAddr(ctx)),
			zap.String("serial", leaf.SerialNumber.String()))
		return nil
	}
}

// UnaryMTLSInterceptor rejects unary calls that do not carry verified mTLS peer
// credentials or whose client tenant is not permitted under the given mode.
func UnaryMTLSInterceptor(logger *zap.Logger, expected certs.Scope, mode OrgMode) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if err := CheckMTLS(ctx, logger, expected, mode); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamMTLSInterceptor rejects streaming calls that do not carry verified mTLS peer
// credentials or whose client organization is not permitted under the given mode.
func StreamMTLSInterceptor(logger *zap.Logger, expected certs.Scope, mode OrgMode) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := CheckMTLS(ss.Context(), logger, expected, mode); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}
