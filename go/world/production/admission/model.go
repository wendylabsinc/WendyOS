// Package admission applies separately commissioned World policy and durable
// replay protection after Wendy request-proof verification. It exposes no route.
package admission

import (
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

const PolicyTyp = "wendy.world.site-policy.v1"
const RevocationsTyp = "wendy.world.revocations.v1"
const ReadScope = "world.read"

var (
	ErrDenied   = errors.New("site admission denied")
	ErrReplay   = errors.New("request nonce already admitted")
	ErrRollback = errors.New("policy, revocation or trusted-time rollback")
	ErrConflict = errors.New("conflicting content at retained revision")
	ErrCapacity = errors.New("admission ledger quota exhausted")
	ErrClosed   = errors.New("admission ledger closed")
	ErrFaulted  = errors.New("admission persistence uncertain; reopen required")
)

type Pin struct {
	Principal        string `json:"principal"`
	SigningKeySHA256 string `json:"signing_key_sha256"`
}

type Config struct {
	Tenant, Site, Audience                            string
	PolicyIssuer, RevocationIssuer                    Pin
	Certificates                                      *wendycert.Verifier
	MaxRevocationAge                                  time.Duration // required, finite commissioned limit
	MaxPolicyLifetime, MaxRequestLifetime             time.Duration
	MaxNonces, MaxPolicyEntries, MaxRevoked, MaxBytes int
}

type PolicyEntry struct {
	Principal string   `json:"principal"`
	Scopes    []string `json:"scopes"`
}
type Policy struct {
	Version         int           `json:"version"`
	Domain          string        `json:"domain"`
	Tenant          string        `json:"tenant"`
	Site            string        `json:"site"`
	Audience        string        `json:"audience"`
	IssuerPrincipal string        `json:"issuer_principal"`
	IssuerKeySHA256 string        `json:"issuer_key_sha256"`
	PolicyID        string        `json:"policy_id"`
	Revision        uint64        `json:"revision"`
	IssuedAtUnixMS  int64         `json:"issued_at_unix_ms"`
	ExpiresAtUnixMS int64         `json:"expires_at_unix_ms"`
	Entries         []PolicyEntry `json:"entries"`
}
type Revocations struct {
	Version                  int      `json:"version"`
	Domain                   string   `json:"domain"`
	Tenant                   string   `json:"tenant"`
	Site                     string   `json:"site"`
	Audience                 string   `json:"audience"`
	IssuerPrincipal          string   `json:"issuer_principal"`
	IssuerKeySHA256          string   `json:"issuer_key_sha256"`
	Revision                 uint64   `json:"revision"`
	IssuedAtUnixMS           int64    `json:"issued_at_unix_ms"`
	ExpiresAtUnixMS          int64    `json:"expires_at_unix_ms"`
	RevokedPrincipals        []string `json:"revoked_principals"`
	RevokedKeySHA256         []string `json:"revoked_key_sha256"`
	RevokedCertificateSHA256 []string `json:"revoked_certificate_sha256"`
	RevokedPolicyIDs         []string `json:"revoked_policy_ids"`
}

// Request is the checked projection of a cryptographically verified request.
// The caller must verify its signature, route and raw body against commissioned
// expectations throughout the supplied interval before invoking Admit.
type Request struct {
	Identity                                     wendycert.Identity
	Tenant, Site, Audience, Principal, Operation string
	PolicyRevision                               uint64
	Nonce                                        string
	IssuedAtUnixMS, ExpiresAtUnixMS              int64
}
type Snapshot struct {
	PolicyRevision, RevocationRevision       uint64
	PolicyDigest, RevocationDigest, PolicyID string
	NonceCount                               int
	EarliestCheckpoint                       time.Time
}

type retained struct {
	Revision   uint64               `json:"revision"`
	Digest     string               `json:"digest"`
	Proof      string               `json:"proof"`
	VerifiedAt trustedtime.Interval `json:"verified_at"`
}
