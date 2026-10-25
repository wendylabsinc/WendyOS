package platform

import (
	"context"
	"time"
)

// LiteLink exchanges serialized WendyCom messages over an established host
// connection. The host owns serial/USB/BLE access, framing, TLS and deadlines.
// Send is safe for concurrent calls; Receive has one reader. Close interrupts
// blocked operations. A nonpositive Receive timeout means no deadline. Send
// borrows immutable bytes until it returns; Receive transfers owned bytes.
type LiteLink interface {
	Handshake() error
	Send(message []byte) error
	Receive(timeout time.Duration) ([]byte, error)
	PreferredChunkSize() int
	Close() error
}

// LiteCertificate contains public X.509 facts inspected by the host. Core uses
// them for firmware compatibility and CA selection, never chain verification.
type LiteCertificate struct {
	Raw, RawIssuer, RawSubject                           []byte
	BasicConstraintsValid, IsCA                          bool
	NotBefore, NotAfter                                  time.Time
	PublicKeyAlgorithm, SignatureAlgorithm, SignatureOID string
	P256, SelfSignatureValid                             bool
}

// LiteCertificateInspector parses public DER without retaining private keys.
type LiteCertificateInspector interface {
	InspectCertificate(der []byte) (*LiteCertificate, error)
}

// LiteHTTPResponse includes only host-verified TLS chains. An unverified
// connection must return no chains; Core refuses it during CA discovery.
type LiteHTTPResponse struct {
	HTTPResponse
	VerifiedChains [][]*LiteCertificate
}

// LiteHTTPTransport uses native HTTPS with host-configured trust anchors and
// no redirects or retries.
// Cancellation bounds the exchange and response-body reads.
type LiteHTTPTransport interface {
	DoLite(ctx context.Context, request HTTPRequest) (*LiteHTTPResponse, error)
}

// LiteTimeServer identifies one of the firmware-supported Roughtime servers.
type LiteTimeServer struct{ Name, Address string }

// LiteTimeSource obtains signed, nonce-bound evidence using native networking.
// Core forwards bytes unchanged; firmware verifies signatures and consensus.
type LiteTimeSource interface {
	Servers() []LiteTimeServer
	Query(ctx context.Context, server LiteTimeServer, nonce []byte) ([]byte, error)
}
