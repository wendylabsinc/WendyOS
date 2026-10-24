package platform

import "context"

// DeviceIdentity is an expected peer identity, checked by the host's TLS
// verifier. TenantUUID identifies PKI principals; OrgID supports legacy peers.
type DeviceIdentity struct {
	OrgID      int32
	TenantUUID string
	EntityType string
	EntityID   string
	// Principal is the full SPIFFE URI when known. Hosts must compare it
	// exactly when non-empty, preserving the principal kind and complete path.
	// Matching TenantUUID/EntityType/EntityID alone is insufficient: device/<id>
	// and service/asset-<id> can project to the same legacy asset identity.
	Principal string
}

// DeviceConnectRequest describes one connection attempt, never a retry policy.
// Credential is an opaque host reference, not private key material. The host
// resolves it to the certificate, signer and TLS configuration it owns.
type DeviceConnectRequest struct {
	Address    string
	TLS        bool
	Credential string
	Expected   *DeviceIdentity
}

// DeviceConnectionEvidence is a snapshot after an RPC probe. ObservedOrg is an
// untrusted ordering hint from a peer certificate, never authority. Mismatch
// errors come only from the host verifier and must retain their original types.
// TCPDialError is non-nil only when TCP never connected, before TLS began.
type DeviceConnectionEvidence struct {
	ObservedOrg      int32
	IdentityMismatch error
	PinMismatch      error
	TCPDialError     error
}

// DeviceConnection is one host-owned connection and its RPC transport. Evidence
// must be safe to read concurrently with transport activity. Close releases the
// connection; it must be safe after cancellation and must not block indefinitely.
// Transport lifetime is independent of the context used to open the connection.
type DeviceConnection interface {
	Transport
	Evidence() DeviceConnectionEvidence
	Close() error
}

// DeviceConnector opens exactly the endpoint requested by Core. The host owns
// sockets, TLS, certificate verification, pin persistence and session caching.
// It must be safe for concurrent attempts, honor cancellation, and neither
// retry nor select another endpoint or credential. TLS connections must verify
// Expected (including its exact Principal when non-empty) and the host's pin
// policy before accepting any RPC.
// A nil error transfers connection ownership to Core, including lazy dialers.
type DeviceConnector interface {
	ConnectDevice(context.Context, DeviceConnectRequest) (DeviceConnection, error)
}

// DevicePeerRejectedError carries a verifier rejection from a host that performs
// its handshake eagerly inside ConnectDevice. Lazy connections report the same
// underlying cause through Evidence instead. Either form forbids unauthenticated
// fallback. Cause must retain the original identity or SPKI mismatch error.
type DevicePeerRejectedError struct{ Cause error }

func (e *DevicePeerRejectedError) Error() string { return e.Cause.Error() }
func (e *DevicePeerRejectedError) Unwrap() error { return e.Cause }
