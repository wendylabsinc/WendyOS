// Package meshcatalog holds origin-signed, leased app services for one mesh.
// Multicast observations are not admitted as ownership or retransmitted.
package meshcatalog

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

const (
	MaxRecordBytes = 8192
	MaxLease       = 2 * time.Minute
	MaxTXTBytes    = 1200
	MaxTXTEntries  = 32
	recordDomain   = "wendy-mesh-service/record/1\x00"
	// GatewayAppID and GatewayServiceID identify the agent-owned capability.
	// It is never an app service or a DNS-SD endpoint.
	GatewayAppID      = "wendy.mesh.system"
	GatewayServiceID  = "internet-gateway"
	GatewayOfferLease = 600 * time.Second
)

var (
	ErrExpired        = errors.New("expired mesh service record")
	ErrStale          = errors.New("stale mesh service generation")
	ErrIdentityNeeded = errors.New("mesh service identity bundle needed")
	labelPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	typePattern       = regexp.MustCompile(`^_[A-Za-z0-9][A-Za-z0-9-]{0,14}\._(tcp|udp)$`)
)

// Key is a service's stable owner scope. A display name and TXT UUID are never
// used as identity. Generation increases for both publish and remove.
type Key struct {
	Mesh      string `json:"mesh"`
	Org       int32  `json:"org"`
	Asset     int32  `json:"asset"`
	AppID     string `json:"appId"`
	ServiceID string `json:"serviceId"`
}

type Record struct {
	Version    int      `json:"version"`
	Key        Key      `json:"key"`
	Generation uint64   `json:"generation"`
	Type       string   `json:"type"`
	Instance   string   `json:"instance"`
	HostPort   uint16   `json:"hostPort"`
	TXT        []string `json:"txt,omitempty"`
	Issued     int64    `json:"issued"`
	Expires    int64    `json:"expires"`
	Withdraw   bool     `json:"withdraw,omitempty"`
}

type SignedRecord struct {
	Fingerprint string          `json:"fingerprint"`
	Body        json.RawMessage `json:"body"`
	Signature   []byte          `json:"signature"`
}

// IsGatewayOffer identifies the exact reserved, endpoint-free system record.
// Callers must obtain records from a Catalog to rely on signature and lease
// verification; this shape check alone does not authenticate a record.
func IsGatewayOffer(r Record) bool {
	return r.Version == 1 && r.Key.AppID == GatewayAppID &&
		r.Key.ServiceID == GatewayServiceID && r.Type == "" &&
		r.Instance == "" && r.HostPort == 0 && len(r.TXT) == 0
}

func (r Record) Validate(now time.Time) error {
	if r.Version != 1 || !labelPattern.MatchString(r.Key.Mesh) || r.Key.Org <= 0 ||
		r.Key.Asset <= 0 || r.Key.Asset > 65534 || appconfig.ValidateAppID(r.Key.AppID) != nil ||
		!labelPattern.MatchString(r.Key.ServiceID) || r.Generation == 0 {
		return errors.New("invalid mesh service owner or generation")
	}
	issued, expires := time.UnixMilli(r.Issued), time.UnixMilli(r.Expires)
	if !expires.After(now) {
		return ErrExpired
	}
	leaseLimit := MaxLease
	if r.Key.AppID == GatewayAppID && r.Key.ServiceID == GatewayServiceID {
		leaseLimit = GatewayOfferLease
	}
	if issued.After(now.Add(5*time.Second)) || !expires.After(issued) || expires.Sub(issued) > leaseLimit {
		return errors.New("invalid mesh service lease")
	}
	if r.Key.AppID == GatewayAppID || r.Key.ServiceID == GatewayServiceID {
		if !IsGatewayOffer(r) {
			return errors.New("invalid reserved mesh gateway offer")
		}
		return nil
	}
	if r.Withdraw {
		if r.HostPort != 0 || r.Type != "" || r.Instance != "" || len(r.TXT) != 0 {
			return errors.New("mesh service withdrawal carries endpoint data")
		}
		return nil
	}
	if !typePattern.MatchString(r.Type) || r.HostPort == 0 || len(r.Instance) == 0 || len(r.Instance) > 63 ||
		strings.ContainsAny(r.Instance, "\x00\r\n") {
		return errors.New("invalid mesh service endpoint")
	}
	if len(r.TXT) > MaxTXTEntries {
		return errors.New("too many mesh service TXT entries")
	}
	total := 0
	for _, item := range r.TXT {
		if len(item) == 0 || len(item) > 255 || strings.ContainsAny(item, "\x00\r\n") {
			return errors.New("invalid mesh service TXT entry")
		}
		total += len(item) + 1
	}
	if total > MaxTXTBytes {
		return errors.New("mesh service TXT too large")
	}
	return nil
}

func signInput(key crypto.PublicKey, body []byte) ([]byte, crypto.SignerOpts) {
	input := append([]byte(recordDomain), body...)
	if _, ok := key.(ed25519.PublicKey); ok {
		return input, crypto.Hash(0)
	}
	h := sha256.Sum256(input)
	return h[:], crypto.SHA256
}

func Sign(r Record, chain [][]byte, signer crypto.Signer, now time.Time) (SignedRecord, error) {
	var out SignedRecord
	if err := r.Validate(now); err != nil {
		return out, err
	}
	if signer == nil || len(chain) == 0 {
		return out, errors.New("missing mesh service signer")
	}
	body, err := json.Marshal(r)
	if err != nil {
		return out, err
	}
	if len(body) > MaxRecordBytes {
		return out, errors.New("oversized mesh service record")
	}
	input, opts := signInput(signer.Public(), body)
	sig, err := signer.Sign(rand.Reader, input, opts)
	if err != nil {
		return out, err
	}
	return SignedRecord{localmesh.Fingerprint(chain), body, sig}, nil
}

func verify(w SignedRecord, cache *localmesh.IdentityCache, now time.Time) (Record, string, error) {
	var r Record
	if len(w.Body) == 0 || len(w.Body) > MaxRecordBytes || len(w.Signature) == 0 ||
		len(w.Signature) > 2048 || len(w.Fingerprint) != 64 {
		return r, "", errors.New("invalid mesh service envelope")
	}
	if err := json.Unmarshal(w.Body, &r); err != nil {
		return r, "", err
	}
	if err := r.Validate(now); err != nil {
		return r, "", err
	}
	chain, id, ok := cache.Get(w.Fingerprint, now)
	if !ok {
		return r, "", ErrIdentityNeeded
	}
	if id.Org != r.Key.Org || id.Asset != r.Key.Asset || time.UnixMilli(r.Expires).After(id.NotAfter) {
		return r, "", errors.New("mesh service origin or lease does not match certificate")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return r, "", err
	}
	input, _ := signInput(leaf.PublicKey, w.Body)
	valid := false
	switch key := leaf.PublicKey.(type) {
	case ed25519.PublicKey:
		valid = ed25519.Verify(key, input, w.Signature)
	case *ecdsa.PublicKey:
		valid = ecdsa.VerifyASN1(key, input, w.Signature)
	case *rsa.PublicKey:
		valid = rsa.VerifyPKCS1v15(key, crypto.SHA256, input, w.Signature) == nil
	}
	if !valid {
		return r, "", errors.New("invalid mesh service signature")
	}
	h := sha256.Sum256(w.Body)
	return r, hex.EncodeToString(h[:]), nil
}

// PublishSpec comes only from an already identified local app. HostPort must
// be a live ingress mapping owned by that app; the injected policy enforces it.
type PublishSpec struct {
	AppID, ServiceID, Type, Instance string
	HostPort                         uint16
	TXT                              []string
	Lease                            time.Duration
}

// AuthorizePublication must check the app's network mode, declared service
// type and its live ingress ownership. It is invoked only for local writes.
type AuthorizePublication func(appID, serviceType string, hostPort uint16) error

func (s PublishSpec) record(key Key, generation uint64, now time.Time) (Record, error) {
	lease := s.Lease
	if lease == 0 {
		lease = MaxLease
	}
	if lease < time.Second || lease > MaxLease {
		return Record{}, fmt.Errorf("mesh service lease must be 1s..%s", MaxLease)
	}
	r := Record{Version: 1, Key: key, Generation: generation, Type: s.Type,
		Instance: s.Instance, HostPort: s.HostPort, TXT: append([]string(nil), s.TXT...),
		Issued: now.UnixMilli(), Expires: now.Add(lease).UnixMilli()}
	return r, r.Validate(now)
}
