// Package sourceauthority certifies bounded remote publication authority.
// It grants neither observation freshness nor robot or task authority.
package sourceauthority

import (
	"context"
	"crypto/mldsa"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

const (
	Version           = 1
	MaxLifetime       = 30 * time.Second
	MaxClaimsBytes    = 16 << 10
	MaxEnvelopeBytes  = 128 << 10
	leasePurpose      = "wendy.world.source-lease.v1\x00"
	floorPurpose      = "wendy.world.source-revocation-floor.v1\x00"
	delegationPurpose = "wendy.world.service-delegation.v1\x00"
)

var (
	ErrDenied         = errors.New("source authority denied")
	ErrExpired        = errors.New("source authority expired")
	ErrReplay         = errors.New("source authority replay or conflict")
	ErrClock          = errors.New("source authority clock unavailable or regressed")
	ErrCapacity       = errors.New("source authority capacity exceeded")
	ErrCorrupt        = errors.New("source authority storage integrity failure")
	ErrNotInitialized = errors.New("source authority not initialized")
	ErrClosed         = errors.New("source authority closed")
	ErrFaulted        = errors.New("source authority outcome uncertain; reopen required")
)

// Source is the durable counter key, independent of certificate renewal,
// deployment generation, publication key and source epoch.
type Source struct {
	Tenant  string `json:"tenant"`
	Device  string `json:"device"`
	App     string `json:"app"`
	Service string `json:"service"`
}

type LeaseClaims struct {
	Version           int                 `json:"version"`
	AuthorityDomain   string              `json:"authority_domain"`
	Scope             serviceorigin.Scope `json:"scope"`
	SampleClockDomain string              `json:"sample_clock_domain"`
	Epoch             uint64              `json:"epoch"`
	DelegationSHA256  string              `json:"delegation_sha256"`
	AuthorityRevision uint64              `json:"authority_revision"`
	NotBeforeUnixMS   int64               `json:"not_before_unix_ms"`
	NotAfterUnixMS    int64               `json:"not_after_unix_ms"`
}

// FloorClaims is a permanent denial after durable acceptance. Its time window
// bounds initial authentication, not the lifetime of the retained denial.
type FloorClaims struct {
	Version             int    `json:"version"`
	AuthorityDomain     string `json:"authority_domain"`
	Source              Source `json:"source"`
	Site                string `json:"site"`
	ExecutionDomain     string `json:"execution_domain"`
	SampleClockDomain   string `json:"sample_clock_domain"`
	ThroughRevision     uint64 `json:"through_revision"`
	AttestationRevision uint64 `json:"attestation_revision"`
	MinimumEpoch        uint64 `json:"minimum_epoch"`
	NotBeforeUnixMS     int64  `json:"not_before_unix_ms"`
	NotAfterUnixMS      int64  `json:"not_after_unix_ms"`
}

// Claims are exact canonical signed bytes, never reconstructed for signature
// verification. Envelope encoding is canonical too; transport retains it.
type SignedLease struct {
	Claims    []byte   `json:"claims"`
	ChainDER  [][]byte `json:"chain_der"`
	Signature []byte   `json:"signature"`
}
type SignedFloor struct {
	Claims    []byte   `json:"claims"`
	ChainDER  [][]byte `json:"chain_der"`
	Signature []byte   `json:"signature"`
}

// Callbacks must enforce exact commissioned grants, current binding on the
// origin, and all certificate/key deny-lists. They run while Store is locked;
// they must not reenter it or acquire a journal lock. Enrollment is not a grant.
type AuthorizeLease func(context.Context, LeaseClaims, wendycert.DeviceIdentity) error
type AuthorizeFloor func(context.Context, FloorClaims, wendycert.DeviceIdentity) error
type Config struct {
	Path                                             string
	Tenant, Site, ExecutionDomain, SampleClockDomain string
	Clock                                            trustedtime.Provider
	Certificates                                     *wendycert.DeviceVerifier
	AuthorizeLease                                   AuthorizeLease
	AuthorizeFloor                                   AuthorizeFloor
	MaxSources                                       int
	MaxBytes                                         int
}

// Signer is only an agent injection seam. No generic signing method or key
// export is exposed. The host holds its identity/lifecycle fence throughout
// issuance and stops old-binding issuance before issuing a revocation floor.
type Signer struct {
	ChainDER  [][]byte
	DeviceKey *mldsa.PrivateKey
}
type LeaseRequest struct {
	Delegation serviceorigin.SignedDelegation
	Lifetime   time.Duration
}

type State struct {
	Source          Source `json:"source"`
	Revision        uint64 `json:"revision"`
	LeaseRevision   uint64 `json:"lease_revision"`
	Epoch           uint64 `json:"epoch"`
	BindingSHA256   string `json:"binding_sha256,omitempty"`
	ThroughRevision uint64 `json:"through_revision"`
	FloorRevision   uint64 `json:"floor_revision"`
	MinimumEpoch    uint64 `json:"minimum_epoch"`
	Lease           []byte `json:"lease,omitempty"`
	Floor           []byte `json:"floor,omitempty"`
}

func SourceOf(scope serviceorigin.Scope) Source {
	return Source{scope.Tenant, scope.Device, scope.App, scope.Service}
}
