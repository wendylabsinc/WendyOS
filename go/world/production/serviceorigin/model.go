// Package serviceorigin attributes structured World publications to an enrolled
// device and one locally commissioned app service. It provides neither site
// commissioning nor replica admission, mission authority, or hardware control.
package serviceorigin

import (
	"context"
	"crypto/mldsa"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

const (
	Version          = 1
	AuthorityDomain  = "physical"
	MaxLifetime      = time.Hour
	MaxRecordBytes   = 64 << 10
	MaxPayloadBytes  = 32 << 10
	MaxClaimsBytes   = 16 << 10
	MaxPublishScopes = 64
	delegationPrefix = "wendy.world.service-delegation.v1\x00"
	recordPrefix     = "wendy.world.service-record.v1\x00"
)

var (
	ErrDenied  = errors.New("service publication denied")
	ErrClosed  = errors.New("service publisher closed")
	ErrExpired = errors.New("service publication authority expired")
	ErrClock   = errors.New("service publication clock unavailable or regressed")
)

// PublishScope is an exact component/stream pair. Wildcards are not supported.
type PublishScope struct {
	ComponentType string `json:"component_type"`
	Stream        string `json:"stream"`
}

// Scope comes only from trusted commissioning and the container lifecycle.
// DeploymentGeneration changes on deployment/entitlement replacement. It is
// distinct from the durable source Epoch, which changes on publisher restart.
// Empty Publish is a no-write authority lease used by read-only app sockets.
// validateRequest still requires an exact listed pair for every publication.
type Scope struct {
	Tenant               string         `json:"tenant"`
	Site                 string         `json:"site"`
	Device               string         `json:"device"`
	App                  string         `json:"app"`
	Service              string         `json:"service"`
	DeploymentGeneration uint64         `json:"deployment_generation"`
	ExecutionDomain      string         `json:"execution_domain"`
	Publish              []PublishScope `json:"publish"`
}

// Validate checks the exact service identity and publication-scope syntax. It
// does not grant site permission or establish enrollment authenticity.
func (s Scope) Validate() error { return validateScope(s) }

// PublishRequest has no caller-controlled identity or version. Source timing
// and provenance are preserved, not replaced with the physical signing clock.
// Payload is bytes so JSON transport cannot normalize its original encoding.
type PublishRequest struct {
	Entity        string           `json:"entity"`
	ComponentType string           `json:"component_type"`
	Stream        string           `json:"stream"`
	Frame         world.FrameRef   `json:"frame"`
	Timing        world.Timing     `json:"timing"`
	TTL           time.Duration    `json:"ttl_ns"`
	Provenance    world.Provenance `json:"provenance"`
	Payload       []byte           `json:"payload,omitempty"`
	Tombstone     bool             `json:"tombstone"`
}

type DelegationClaims struct {
	Version         int    `json:"version"`
	AuthorityDomain string `json:"authority_domain"`
	Scope           Scope  `json:"scope"`
	Epoch           uint64 `json:"epoch"`
	IssuedAtUnixMS  int64  `json:"issued_at_unix_ms"`
	ExpiresAtUnixMS int64  `json:"expires_at_unix_ms"`
	PublicKey       []byte `json:"public_key"`
}

// Claims and Record contain immutable signed JSON bytes. They are encoded as
// base64 by encoding/json and are never reconstructed for verification.
type SignedDelegation struct {
	Claims    []byte   `json:"claims"`
	ChainDER  [][]byte `json:"chain_der"`
	Signature []byte   `json:"signature"`
}
type SignedRecord struct {
	Delegation SignedDelegation `json:"delegation"`
	Record     []byte           `json:"record"`
	Signature  []byte           `json:"signature"`
}
type Record struct {
	Version          int            `json:"version"`
	AuthorityDomain  string         `json:"authority_domain"`
	DelegationSHA256 string         `json:"delegation_sha256"`
	Scope            Scope          `json:"scope"`
	Epoch            uint64         `json:"epoch"`
	Sequence         uint64         `json:"sequence"`
	IssuedAtUnixMS   int64          `json:"issued_at_unix_ms"`
	ExpiresAtUnixMS  int64          `json:"expires_at_unix_ms"`
	Publication      PublishRequest `json:"publication"`
}

// PublisherConfig is an in-agent injection seam, not an app-facing request.
// The host must reserve Epoch durably before creating a publisher and must
// close it on entitlement changes, device reset/rotation or revocation.
type PublisherConfig struct {
	Scope        Scope
	Epoch        uint64
	Lifetime     time.Duration
	Clock        trustedtime.Provider
	Certificates *wendycert.DeviceVerifier
	ChainDER     [][]byte
	DeviceKey    *mldsa.PrivateKey
}

// Authorize must enforce commissioned site/service grants and current
// revocation against the exact claims. A valid device certificate is not a
// grant. Implementations must honor context cancellation and fail closed.
type Authorize func(context.Context, DelegationClaims, wendycert.DeviceIdentity) error
type VerifierConfig struct {
	Tenant, Site, ExecutionDomain string
	Certificates                  *wendycert.DeviceVerifier
	Clock                         trustedtime.Provider
	Authorize                     Authorize
}
type VerifiedRecord struct {
	Record     Record
	Bytes      []byte
	Delegation DelegationClaims
	Device     wendycert.DeviceIdentity
}

// Component is a fresh owned projection. Downstream code must still validate
// schema, frame, sample freshness and durable replica high-water marks.
func (v VerifiedRecord) Component() world.Component {
	r := v.Record
	p := r.Publication
	return world.Component{Key: world.Key{Tenant: r.Scope.Tenant, Site: r.Scope.Site, Entity: p.Entity, Type: p.ComponentType, Source: world.Source{Device: r.Scope.Device, App: r.Scope.App, Service: r.Scope.Service, Stream: p.Stream}}, Version: world.Version{Epoch: r.Epoch, Sequence: r.Sequence}, Frame: p.Frame, Timing: p.Timing, TTL: p.TTL, Provenance: cloneRequest(p).Provenance, Payload: append([]byte(nil), p.Payload...), Tombstone: p.Tombstone}
}
