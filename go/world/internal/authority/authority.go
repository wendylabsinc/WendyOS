// Package authority verifies explicitly scoped offline grants and signed requests.
// Its pinned Ed25519 root is an initial trust format, not a SPIFFE/ML-DSA CA.
package authority

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/strictjson"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Version = 1
const MaxEnvelopeBytes = 64 << 10
const DefaultRecordLifetime = 24 * time.Hour

type Grant struct {
	TaskStatusAssignee string       `json:"task_status_assignee,omitempty"`
	TaskStatusAudience string       `json:"task_status_audience,omitempty"`
	ExecuteAssignee    string       `json:"execute_assignee,omitempty"`
	PublishAudience    string       `json:"publish_audience,omitempty"`
	Version            int          `json:"version"`
	ID                 string       `json:"id"`
	Tenant             string       `json:"tenant"`
	Site               string       `json:"site"`
	Domain             string       `json:"domain"`
	Principal          string       `json:"principal"`
	Kind               string       `json:"kind"` // operator, app, agent, device
	Source             world.Source `json:"source"`
	PublicKey          []byte       `json:"public_key"`
	Scopes             []string     `json:"scopes"`
	NotBeforeUnixMS    int64        `json:"not_before_unix_ms"`
	NotAfterUnixMS     int64        `json:"not_after_unix_ms"`
}
type SignedGrant struct {
	Grant     Grant  `json:"grant"`
	Signature []byte `json:"signature"`
}
type Revocations struct {
	Version         int      `json:"version"`
	Tenant          string   `json:"tenant"`
	Site            string   `json:"site"`
	Domain          string   `json:"domain"`
	Revision        uint64   `json:"revision"`
	IssuedAtUnixMS  int64    `json:"issued_at_unix_ms"`
	ExpiresAtUnixMS int64    `json:"expires_at_unix_ms"`
	GrantIDs        []string `json:"grant_ids"`
	KeyIDs          []string `json:"key_ids"`
	Principals      []string `json:"principals"`
}
type SignedRevocations struct {
	Revocations Revocations `json:"revocations"`
	Signature   []byte      `json:"signature"`
}
type SignedRequest struct {
	Version         int         `json:"version"`
	Grant           SignedGrant `json:"grant"`
	Audience        string      `json:"audience"`
	Operation       string      `json:"operation"`
	BodySHA256      string      `json:"body_sha256"`
	Nonce           string      `json:"nonce"`
	IssuedAtUnixMS  int64       `json:"issued_at_unix_ms"`
	ExpiresAtUnixMS int64       `json:"expires_at_unix_ms"`
	Signature       []byte      `json:"signature"`
}
type SignedRecord struct {
	Version         int             `json:"version"`
	Grant           SignedGrant     `json:"grant"`
	Component       world.Component `json:"component"`
	SignedAtUnixMS  int64           `json:"signed_at_unix_ms"`
	ExpiresAtUnixMS int64           `json:"expires_at_unix_ms"`
	Signature       []byte          `json:"signature"`
}
type Identity struct {
	SigningKeySHA256   string
	PublishAudience    string
	TaskStatusAssignee string
	TaskStatusAudience string
	ExecuteAssignee    string
	Principal          string
	Kind               string
	Source             world.Source
	Scopes             []string
	GrantID            string
	NotAfter           time.Time
}

func (i Identity) Allows(scope string) bool { return contains(i.Scopes, scope) }

// Config.Now must be a trusted wall clock. Observation simulation clocks must
// never be supplied here. Replay must persist when acceptance survives restart.
type Config struct {
	Metrics                                                                   *runtimeobs.Collector
	RootPublicKey                                                             ed25519.PublicKey
	Tenant, Site, Domain, Audience                                            string
	Now                                                                       func() time.Time
	MaxGrantLifetime, MaxRequestLifetime, MaxRevocationAge, MaxRecordLifetime time.Duration
	MaxBodyBytes                                                              int
	Replay                                                                    ReplayStore
}
type Verifier struct {
	mu  sync.Mutex
	cfg Config
	rev *SignedRevocations
}

// MatchesScope checks immutable commissioning, not a caller's authorization.
func (v *Verifier) MatchesScope(tenant, site, domain, audience string) bool {
	return v != nil && v.cfg.Tenant == tenant && v.cfg.Site == site && v.cfg.Domain == domain && v.cfg.Audience == audience
}

var (
	ErrDenied           = errors.New("authority denied")
	ErrReplay           = errors.New("request nonce already consumed")
	ErrStaleRevocations = errors.New("revocation information absent or stale")
)

func NewVerifier(c Config) (*Verifier, error) {
	if len(c.RootPublicKey) != ed25519.PublicKeySize || c.Tenant == "" || c.Site == "" || c.Audience == "" || !validDomain(c.Domain) || c.Now == nil || c.Replay == nil {
		return nil, errors.New("invalid authority configuration")
	}
	if c.MaxGrantLifetime == 0 {
		c.MaxGrantLifetime = 24 * time.Hour
	}
	if c.MaxRequestLifetime == 0 {
		c.MaxRequestLifetime = 5 * time.Minute
	}
	if c.MaxRevocationAge == 0 {
		c.MaxRevocationAge = time.Hour
	}
	if c.MaxRecordLifetime == 0 {
		c.MaxRecordLifetime = DefaultRecordLifetime
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 1 << 20
	}
	if c.MaxRecordLifetime < time.Millisecond || c.MaxGrantLifetime < time.Millisecond || c.MaxRequestLifetime < time.Millisecond || c.MaxRevocationAge < time.Millisecond || c.MaxBodyBytes < 1 || c.MaxBodyBytes > 16<<20 {
		return nil, errors.New("invalid authority limits")
	}
	c.RootPublicKey = append(ed25519.PublicKey(nil), c.RootPublicKey...)
	return &Verifier{cfg: c}, nil
}

// ValidateRevocations checks an update without changing replay or verifier state.
// A server can persist its body before calling UpdateRevocations. Callers must
// serialize that sequence with every other revocation update.
func (v *Verifier) ValidateRevocations(s SignedRevocations) error {
	v.lock()
	defer v.mu.Unlock()
	_, err := v.validateRevocations(s, v.cfg.Now())
	return err
}
func (v *Verifier) validateRevocations(s SignedRevocations, now time.Time) ([]byte, error) {
	r := s.Revocations
	if !verify(v.cfg.RootPublicKey, "revocations", r, s.Signature) || r.Version != Version || r.Tenant != v.cfg.Tenant || r.Site != v.cfg.Site || r.Domain != v.cfg.Domain || r.Revision == 0 {
		return nil, ErrDenied
	}
	if !uniqueBounded(r.GrantIDs, 4096) || !uniqueBounded(r.KeyIDs, 4096) || !uniqueBounded(r.Principals, 4096) {
		return nil, ErrDenied
	}
	if !validWindow(r.IssuedAtUnixMS, r.ExpiresAtUnixMS, v.cfg.MaxRevocationAge) || now.UnixMilli() < r.IssuedAtUnixMS || now.UnixMilli() >= r.ExpiresAtUnixMS {
		return nil, ErrStaleRevocations
	}
	encoded, _ := json.Marshal(s)
	if len(encoded) > 1<<20 {
		return nil, ErrDenied
	}
	if err := v.cfg.Replay.ValidateCheckpoint(r.Revision, hash(encoded), now); err != nil {
		return nil, err
	}
	return encoded, nil
}
func (v *Verifier) UpdateRevocations(s SignedRevocations) error {
	v.lock()
	defer v.mu.Unlock()
	now := v.cfg.Now()
	encoded, err := v.validateRevocations(s, now)
	if err != nil {
		return err
	}
	if err = v.cfg.Replay.Checkpoint(s.Revocations.Revision, hash(encoded), now); err != nil {
		return err
	}
	var clone SignedRevocations
	_ = json.Unmarshal(encoded, &clone)
	v.rev = &clone
	return nil
}

func (v *Verifier) Verify(r SignedRequest, operation string, body []byte) (Identity, error) {
	return v.VerifyContext(context.Background(), r, operation, body)
}
func (v *Verifier) VerifyContext(ctx context.Context, r SignedRequest, operation string, body []byte) (Identity, error) {
	hold := v.lockContext(ctx)
	defer v.mu.Unlock()
	defer hold.End()
	now := v.cfg.Now()
	if len(body) > v.cfg.MaxBodyBytes || r.Version != Version || r.Audience != v.cfg.Audience || r.Operation != operation || !validOperation(operation) || r.BodySHA256 != hash(body) || !validNonce(r.Nonce) {
		return Identity{}, ErrDenied
	}
	if !validWindow(r.IssuedAtUnixMS, r.ExpiresAtUnixMS, v.cfg.MaxRequestLifetime) || now.UnixMilli() < r.IssuedAtUnixMS || now.UnixMilli() >= r.ExpiresAtUnixMS {
		return Identity{}, ErrDenied
	}
	if !verify(r.Grant.Grant.PublicKey, "request", requestPayload(r), r.Signature) {
		return Identity{}, ErrDenied
	}
	if (operation == "publish" || operation == "evidence-write" || operation == "evidence-pin" || operation == "site-publish" || operation == "annotation-write") && r.Grant.Grant.PublishAudience != v.cfg.Audience {
		return Identity{}, ErrDenied
	}
	if (operation == "task-status" || operation == "supervision-server") && r.Grant.Grant.TaskStatusAudience != v.cfg.Audience {
		return Identity{}, ErrDenied
	}
	id, checkpoint, err := v.validateIdentity(r.Grant, operation, now, now)
	if err != nil {
		return Identity{}, err
	}
	// Nonces are unique for a signing key and destination, across operations and grants.
	key := KeyID(r.Grant.Grant.PublicKey) + ":" + r.Audience + ":" + r.Nonce
	if admission, ok := v.cfg.Replay.(ReplayAdmissionStore); ok {
		err = replayAdmission(ctx, admission, checkpoint.revision, checkpoint.digest, key, time.UnixMilli(r.ExpiresAtUnixMS), now)
	} else {
		// Custom stores retain the original two-step contract. Only a store that
		// explicitly implements durable combined admission may omit that write.
		err = replayCheckpoint(ctx, v.cfg.Replay, checkpoint.revision, checkpoint.digest, now)
		if err == nil {
			err = replayConsume(ctx, v.cfg.Replay, key, time.UnixMilli(r.ExpiresAtUnixMS), now)
		}
	}
	if err != nil {
		return Identity{}, err
	}
	return id, nil
}

// VerifyRecord authenticates the origin, not a relay. It intentionally does not
// consume a nonce: records are immutable and replica versions handle duplicates.
// The signed issuance must be within the publisher grant validity. Current
// revocation and the absolute proof deadline still apply after grant expiry.
func (v *Verifier) VerifyRecord(r SignedRecord) (Identity, error) {
	return v.VerifyRecordContext(context.Background(), r)
}
func (v *Verifier) VerifyRecordContext(ctx context.Context, r SignedRecord) (Identity, error) {
	hold := v.lockContext(ctx)
	defer v.mu.Unlock()
	defer hold.End()
	now := v.cfg.Now()
	if r.Version != Version || !validWindow(r.SignedAtUnixMS, r.ExpiresAtUnixMS, v.cfg.MaxRecordLifetime) || now.UnixMilli() < r.SignedAtUnixMS || now.UnixMilli() >= r.ExpiresAtUnixMS {
		return Identity{}, ErrDenied
	}
	if !verify(r.Grant.Grant.PublicKey, "record", recordPayload(r), r.Signature) {
		return Identity{}, ErrDenied
	}
	op := "publish"
	if annotations.Reserved(r.Component) {
		if err := validateAnnotationRecord(r); err != nil {
			return Identity{}, err
		}
		if err := v.annotationAmendmentProof(r); err != nil {
			return Identity{}, err
		}
		op = "annotation-write"
		if err := v.annotationClockRecord(r, now, false); err != nil {
			return Identity{}, err
		}
	}
	id, err := v.identityContext(ctx, r.Grant, op, time.UnixMilli(r.SignedAtUnixMS), now)
	if err != nil {
		return Identity{}, err
	}
	if r.Component.Key.Type == annotations.TypeV2 || annotations.IsAmendment(r.Component) {
		completed := v.cfg.Now()
		if completed.Before(now) || completed.Round(0).Before(now.Round(0)) {
			return Identity{}, ErrRecordClockRollback
		}
		if completed.UnixMilli() >= r.ExpiresAtUnixMS {
			return Identity{}, ErrDenied
		}
		if _, _, err = v.validateIdentity(r.Grant, op, time.UnixMilli(r.SignedAtUnixMS), completed); err != nil {
			return Identity{}, err
		}
		if err = v.annotationClockRecord(r, completed, false); err != nil {
			return Identity{}, err
		}
	}
	c := r.Component
	if c.Key.Source != id.Source || c.Key.Tenant != v.cfg.Tenant || c.Key.Site != v.cfg.Site || len(c.Payload) > v.cfg.MaxBodyBytes {
		return Identity{}, ErrDenied
	}
	if v.cfg.Domain == "physical" && c.Provenance.Synthetic() {
		return Identity{}, ErrDenied
	}
	id.NotAfter = time.UnixMilli(r.ExpiresAtUnixMS)
	if annotations.Reserved(r.Component) {
		id.NotAfter = v.annotationDeadline(r)
	}
	return id, nil
}

// VerifyGrant rechecks ongoing stream authority without consuming a request nonce.
func (v *Verifier) VerifyGrant(g SignedGrant, operation string) (Identity, error) {
	return v.VerifyGrantContext(context.Background(), g, operation)
}
func (v *Verifier) VerifyGrantContext(ctx context.Context, g SignedGrant, operation string) (Identity, error) {
	hold := v.lockContext(ctx)
	defer v.mu.Unlock()
	defer hold.End()
	now := v.cfg.Now()
	return v.identityContext(ctx, g, operation, now, now)
}

func (v *Verifier) identity(s SignedGrant, op string, grantTime, now time.Time) (Identity, error) {
	return v.identityContext(context.Background(), s, op, grantTime, now)
}
func (v *Verifier) identityContext(ctx context.Context, s SignedGrant, op string, grantTime, now time.Time) (Identity, error) {
	id, checkpoint, err := v.validateIdentity(s, op, grantTime, now)
	if err != nil {
		return Identity{}, err
	}
	if err := replayCheckpoint(ctx, v.cfg.Replay, checkpoint.revision, checkpoint.digest, now); err != nil {
		return Identity{}, err
	}
	return id, nil
}

type identityCheckpoint struct {
	revision uint64
	digest   string
}

// validateIdentity checks signed policy without changing replay state. Callers
// hold v.mu and must durably admit the returned checkpoint before using id.
// Standalone grant/record checks use identity; requests combine this checkpoint
// with nonce admission when the replay store supports that transaction.
func (v *Verifier) validateIdentity(s SignedGrant, op string, grantTime, now time.Time) (Identity, identityCheckpoint, error) {
	g := s.Grant
	if err := validateGrant(g, v.cfg.MaxGrantLifetime); err != nil {
		return Identity{}, identityCheckpoint{}, err
	}
	if !verify(v.cfg.RootPublicKey, "grant", g, s.Signature) || g.Tenant != v.cfg.Tenant || g.Site != v.cfg.Site || g.Domain != v.cfg.Domain || !contains(g.Scopes, op) {
		return Identity{}, identityCheckpoint{}, ErrDenied
	}
	// Expired role grants retain only operator stop authority. Revocation and
	// fresh request validity still apply. Future grants never authorize a stop.
	if grantTime.UnixMilli() < g.NotBeforeUnixMS || (grantTime.UnixMilli() >= g.NotAfterUnixMS && !(op == "stop" && g.Kind == "operator")) {
		return Identity{}, identityCheckpoint{}, ErrDenied
	}
	if v.rev == nil {
		return Identity{}, identityCheckpoint{}, ErrStaleRevocations
	}
	r := v.rev.Revocations
	if now.UnixMilli() < r.IssuedAtUnixMS || now.UnixMilli() >= r.ExpiresAtUnixMS || now.Sub(time.UnixMilli(r.IssuedAtUnixMS)) > v.cfg.MaxRevocationAge {
		return Identity{}, identityCheckpoint{}, ErrStaleRevocations
	}
	if contains(r.GrantIDs, g.ID) || contains(r.Principals, g.Principal) || contains(r.KeyIDs, KeyID(g.PublicKey)) {
		return Identity{}, identityCheckpoint{}, ErrDenied
	}
	encoded, _ := json.Marshal(v.rev)
	return grantIdentity(g), identityCheckpoint{revision: r.Revision, digest: hash(encoded)}, nil
}

func grantIdentity(g Grant) Identity {
	return Identity{SigningKeySHA256: KeyID(g.PublicKey), PublishAudience: g.PublishAudience, TaskStatusAssignee: g.TaskStatusAssignee, TaskStatusAudience: g.TaskStatusAudience, ExecuteAssignee: g.ExecuteAssignee, Principal: g.Principal, Kind: g.Kind, Source: g.Source, Scopes: append([]string(nil), g.Scopes...), GrantID: g.ID, NotAfter: time.UnixMilli(g.NotAfterUnixMS)}
}

func SignGrant(g Grant, key ed25519.PrivateKey) (SignedGrant, error) {
	if err := validateGrant(g, 365*24*time.Hour); err != nil {
		return SignedGrant{}, err
	}
	sig, err := sign(key, "grant", g)
	return SignedGrant{Grant: g, Signature: sig}, err
}
func SignRevocations(r Revocations, key ed25519.PrivateKey) (SignedRevocations, error) {
	if r.Version != Version || r.Revision == 0 || r.Tenant == "" || r.Site == "" || !validDomain(r.Domain) || !validWindow(r.IssuedAtUnixMS, r.ExpiresAtUnixMS, 365*24*time.Hour) {
		return SignedRevocations{}, ErrDenied
	}
	sig, err := sign(key, "revocations", r)
	return SignedRevocations{Revocations: r, Signature: sig}, err
}
func SignRequest(g SignedGrant, key ed25519.PrivateKey, audience, operation string, body []byte, nonce string, issuedAt time.Time, lifetime time.Duration) (SignedRequest, error) {
	if !privateMatches(key, g.Grant.PublicKey) || audience == "" || !validOperation(operation) || !validNonce(nonce) || lifetime < time.Millisecond || lifetime > 365*24*time.Hour {
		return SignedRequest{}, ErrDenied
	}
	r := SignedRequest{Version: Version, Grant: g, Audience: audience, Operation: operation, BodySHA256: hash(body), Nonce: nonce, IssuedAtUnixMS: issuedAt.UnixMilli(), ExpiresAtUnixMS: issuedAt.Add(lifetime).UnixMilli()}
	sig, err := sign(key, "request", requestPayload(r))
	r.Signature = sig
	return r, err
}
func SignRecord(g SignedGrant, key ed25519.PrivateKey, c world.Component, signedAt time.Time) (SignedRecord, error) {
	if c.TTL < time.Millisecond || c.TTL > 365*24*time.Hour || !privateMatches(key, g.Grant.PublicKey) || g.Grant.Source != c.Key.Source || g.Grant.Tenant != c.Key.Tenant || g.Grant.Site != c.Key.Site {
		return SignedRecord{}, ErrDenied
	}
	r := SignedRecord{Version: Version, Grant: g, Component: c, SignedAtUnixMS: signedAt.UnixMilli(), ExpiresAtUnixMS: signedAt.Add(DefaultRecordLifetime).UnixMilli()}
	sig, err := sign(key, "record", recordPayload(r))
	r.Signature = sig
	return r, err
}
func requestPayload(r SignedRequest) any {
	g, _ := json.Marshal(r.Grant)
	return struct {
		Version     int    `json:"version"`
		GrantDigest string `json:"grant_digest"`
		Audience    string `json:"audience"`
		Operation   string `json:"operation"`
		BodyDigest  string `json:"body_digest"`
		Nonce       string `json:"nonce"`
		Issued      int64  `json:"issued"`
		Expires     int64  `json:"expires"`
	}{r.Version, hash(g), r.Audience, r.Operation, r.BodySHA256, r.Nonce, r.IssuedAtUnixMS, r.ExpiresAtUnixMS}
}
func recordPayload(r SignedRecord) any {
	g, _ := json.Marshal(r.Grant)
	c := r.Component
	c.Timing.AgeHint = 0 // unsigned relay hint, never trusted to extend freshness
	return struct {
		Version     int             `json:"version"`
		GrantDigest string          `json:"grant_digest"`
		Component   world.Component `json:"component"`
		SignedAt    int64           `json:"signed_at"`
		ExpiresAt   int64           `json:"expires_at"`
	}{r.Version, hash(g), c, r.SignedAtUnixMS, r.ExpiresAtUnixMS}
}
func validateGrant(g Grant, max time.Duration) error {
	// Investigation pins require an explicit non-agent capability. Automatic task
	// and package reference holds use a separate server-local namespace.
	if contains(g.Scopes, "evidence-pin") && g.Kind != "operator" && g.Kind != "app" && g.Kind != "device" {
		return ErrDenied
	}
	if contains(g.Scopes, "supervise") && (g.Kind != "operator" || !contains(g.Scopes, "stop") || g.Domain != "simulation") {
		return ErrDenied
	}
	if contains(g.Scopes, "supervision-server") && (!contains(g.Scopes, "task-status") || g.Domain != "simulation") {
		return ErrDenied
	}
	if (contains(g.Scopes, "annotation-clock-server") || contains(g.Scopes, "mission-clock-server")) && (g.Kind != "device" || g.Domain != "simulation" || g.PublishAudience == "" || len(g.PublishAudience) > 256) {
		return ErrDenied
	}
	if contains(g.Scopes, "annotation-write") && (g.Kind != "operator" || g.Domain != "simulation") {
		return ErrDenied
	}
	if contains(g.Scopes, "task-status") {
		if g.Kind != "device" || g.TaskStatusAssignee == "" || len(g.TaskStatusAssignee) > 256 || g.TaskStatusAudience == "" || len(g.TaskStatusAudience) > 256 {
			return ErrDenied
		}
	} else if g.TaskStatusAssignee != "" || g.TaskStatusAudience != "" {
		return ErrDenied
	}
	// Geographic placement changes require an explicitly delegated operator.
	// The publish scope and its owning node remain required for record signing.
	if contains(g.Scopes, "site-publish") && (g.Kind != "operator" || !contains(g.Scopes, "publish")) {
		return ErrDenied
	}
	if contains(g.Scopes, "execute") && ((g.Kind != "app" && g.Kind != "device") || g.ExecuteAssignee == "" || len(g.ExecuteAssignee) > 256) {
		return ErrDenied
	}
	if !contains(g.Scopes, "execute") && g.ExecuteAssignee != "" {
		return ErrDenied
	}
	if (contains(g.Scopes, "publish") || contains(g.Scopes, "evidence-write") || contains(g.Scopes, "evidence-pin") || contains(g.Scopes, "annotation-write")) && (g.PublishAudience == "" || len(g.PublishAudience) > 256) {
		return ErrDenied
	}
	if g.Version != Version || g.ID == "" || len(g.ID) > 256 || g.Tenant == "" || g.Site == "" || !validDomain(g.Domain) || g.Principal == "" || len(g.PublicKey) != ed25519.PublicKeySize || g.Source.Validate() != nil || !validWindow(g.NotBeforeUnixMS, g.NotAfterUnixMS, max) || len(g.Scopes) == 0 || !uniqueBounded(g.Scopes, 16) {
		return ErrDenied
	}
	if g.Kind != "operator" && g.Kind != "app" && g.Kind != "agent" && g.Kind != "device" {
		return ErrDenied
	}
	for _, scope := range g.Scopes {
		if !validOperation(scope) || ((scope == "mission" || scope == "stop") && g.Kind != "operator") {
			return ErrDenied
		}
	}
	return nil
}
func validDomain(s string) bool { return s == "simulation" || s == "physical" }
func validOperation(s string) bool {
	switch s {
	case "mission-clock-server", "annotation-clock-server", "publish", "read", "replicate", "propose", "mission", "stop", "execute", "evidence-read", "evidence-write", "evidence-pin", "site-publish", "task-status", "annotation-write", "supervise", "supervision-server":
		return true
	}
	return false
}
func validWindow(start, end int64, max time.Duration) bool {
	return start > 0 && end > start && end-start > 0 && end-start <= max.Milliseconds()
}
func validNonce(n string) bool {
	if len(n) < 16 || len(n) > 128 {
		return false
	}
	for _, c := range n {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func contains(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}
func uniqueBounded(a []string, max int) bool {
	if len(a) > max {
		return false
	}
	seen := map[string]bool{}
	for _, v := range a {
		if v == "" || len(v) > 512 || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}
func hash(b []byte) string    { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func KeyID(key []byte) string { return hash(key) }
func privateMatches(key ed25519.PrivateKey, pub []byte) bool {
	return len(key) == ed25519.PrivateKeySize && bytes.Equal(key[32:], pub)
}
func signingBytes(kind string, payload any) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return append([]byte("wendy.world.authority.v1/"+kind+"\x00"), b...), nil
}
func sign(key ed25519.PrivateKey, kind string, payload any) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid private key")
	}
	b, err := signingBytes(kind, payload)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(key, b), nil
}
func verify(pub []byte, kind string, payload any, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	b, err := signingBytes(kind, payload)
	return err == nil && ed25519.Verify(pub, b, sig)
}

// DecodeStrict rejects unknown/duplicate object fields and trailing JSON. Callers
// must use it for signed envelopes instead of permissive json.Unmarshal.
func DecodeStrict(data []byte, out any) error {
	return DecodeStrictLimit(data, out, MaxEnvelopeBytes)
}

// DecodeStrictLimit keeps the same duplicate/unknown-field checks for bounded
// application bodies larger than a cryptographic envelope.
func DecodeStrictLimit(data []byte, out any, limit int) error {
	return strictjson.DecodeLimit(data, out, limit)
}

// DecodeRequest is a convenience for HTTP authorization headers or request files.
func DecodeRequest(data []byte) (SignedRequest, error) {
	var r SignedRequest
	err := DecodeStrict(data, &r)
	return r, err
}
