package devices

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

func isReachabilityTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range wrapped.Unwrap() {
			if isReachabilityTimeoutError(cause) {
				return true
			}
		}
		return false
	}
	if cause := errors.Unwrap(err); cause != nil {
		return isReachabilityTimeoutError(cause)
	}
	msg := diagnosticText(err)
	return strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "connection timed out") ||
		strings.Contains(msg, "deadline exceeded")
}

// isTLSCertificateError identifies certificate failures, including ambiguous
// peer alerts. It does not establish that refreshing client credentials helps.
func isTLSCertificateError(err error) bool {
	if err == nil {
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range wrapped.Unwrap() {
			if isTLSCertificateError(cause) {
				return true
			}
		}
		return false
	} else if wrapped := errors.Unwrap(err); wrapped != nil {
		return isTLSCertificateError(wrapped)
	}
	msg := diagnosticText(err)
	if strings.Contains(msg, "first record does not look like a TLS handshake") {
		return false
	}
	for _, signal := range []string{
		// Server verification can fail before the peer evaluates our client
		// certificate, especially when we only tried another org's credential.
		"x509:",
		"server certificate belongs to org ",
		"certificate is not valid for client authentication",
		"certificate not valid at current time",
		"certificate has expired",
		"expired certificate",
		"remote error: tls: bad certificate",
		"remote error: tls: certificate required",
	} {
		if strings.Contains(msg, signal) {
			return true
		}
	}
	return false
}

func isCertRejectionError(addr string, err error) bool {
	if err == nil {
		return false
	}
	// A successful TLS handshake can still be followed by the agent rejecting
	// a certificate without clientAuth on the version RPC. Preserve that
	// explicit diagnosis instead of falling through to plaintext. An arbitrary
	// Unauthenticated response does not establish a refreshable cert problem.
	if unauthenticated(err) && CertificateRefreshReason(err) != "" {
		return true
	}
	msg := diagnosticText(err)
	// Explicit TLS rejection signals take precedence over transport details.
	if strings.Contains(msg, "remote error: tls:") || strings.Contains(msg, "certificate required") {
		return true
	}
	// QEMU's user-mode networking accepts on the host before discovering that
	// the guest port is closed. That ends the TLS probe in either EOF or a TCP
	// read reset, without a TLS alert rejecting the certificate. Only exclude
	// these over loopback: elsewhere the same failure may be an on-path reset,
	// and reading that as "not a TLS endpoint" would re-offer plaintext.
	loopbackReadReset := strings.Contains(msg, "handshake failed: read tcp ") &&
		strings.Contains(msg, ": connection reset by peer")
	if isLoopbackHost(addr) && (strings.Contains(msg, "handshake failed: EOF") || loopbackReadReset) {
		return false
	}
	// A plaintext (unprovisioned) agent probed with TLS reports "first record
	// does not look like a TLS handshake", which gRPC wraps inside its
	// "authentication handshake failed" envelope. That is NOT a cert rejection —
	// the server simply isn't a TLS endpoint — so it must not suppress the
	// plaintext fallback. Exclude it explicitly before the broad substring match
	// below would otherwise catch the "authentication handshake failed" wrapper.
	if strings.Contains(msg, "first record does not look like a TLS handshake") {
		return false
	}
	return strings.Contains(msg, "authentication handshake failed")
}

func unauthenticated(err error) bool {
	var status *platform.RPCStatusError
	return errors.As(err, &status) && status.Code == platform.RPCUnauthenticated
}

func isLoopbackHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

// CertificateRefreshReason only identifies a specific clientAuth capability
// failure. Generic peer expiry alerts alone do not justify replacing credentials.
// Local expiry evidence is supplied separately by ExpiredClientCertificateReason.
func CertificateRefreshReason(err error) string {
	if err == nil {
		return ""
	}
	if wrapped, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range wrapped.Unwrap() {
			if reason := CertificateRefreshReason(cause); reason != "" {
				return reason
			}
		}
		return ""
	}
	if cause := errors.Unwrap(err); cause != nil {
		return CertificateRefreshReason(cause)
	}
	if strings.Contains(diagnosticText(err), "certificate is not valid for client authentication") {
		return "The client certificate does not permit client authentication."
	}
	return ""
}

// RPCStatusError deliberately redacts Error(). Policy must inspect the status
// description, including for non-Go hosts that cannot supply a native Cause.
// This text is never returned as a public error or logged here.
func diagnosticText(err error) string {
	if status, ok := err.(*platform.RPCStatusError); ok {
		return status.Message
	}
	return err.Error()
}

// IsTLSCertificateError distinguishes certificate failures from non-TLS peers.
func IsTLSCertificateError(err error) bool { return isTLSCertificateError(err) }

// IsReachabilityTimeoutError reports the CLI's bounded handshake retry class.
func IsReachabilityTimeoutError(err error) bool { return isReachabilityTimeoutError(err) }

// StopRecovery prevents a retry after cancellation or a host identity refusal.
// HostRefusal is evidence from the host verifier or an explicit user refusal.
func StopRecovery(ctx context.Context, err error, hostRefusal bool) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || hostRefusal
}
