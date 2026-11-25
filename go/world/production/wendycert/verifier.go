// Package wendycert verifies existing Wendy operator certificate proofs.
// Identity is authentication only, never site permission or hardware authority.
package wendycert

import (
	"crypto/mldsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net/url"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/world/production/internal/upstreamcerts"
)

const MaxCertificates = 8
const MaxCertificateBytes = 64 << 10

type Config struct {
	Tenant string
	Roots  [][]byte
}

type Verifier struct {
	tenant string
	roots  []*x509.Certificate
}

// Identity hashes exact certificate DER and canonical marshaled public-key SPKI
// with lowercase SHA-256 hex. Alternate accepted certificate encodings cannot
// give the same signing key a fresh replay or revocation identity.
// The private verification key prevents callers from manufacturing a usable
// Identity by assigning public fields. Revocation and site policy are separate.
type Identity struct {
	Principal         string
	Tenant            string
	SigningKeySHA256  string
	CertificateSHA256 string
	// ValidFrom is the checked interval's lower bound. ValidUntil is the
	// earliest expiry on a successful path, excluding unused issuers/roots.
	ValidFrom             time.Time
	ValidUntil            time.Time
	key                   *mldsa.PublicKey
	candidateCertificates []string
	candidateKeys         []string
}

// CandidateCertificateSHA256 returns all presented certificates and configured
// anchors, not just the selected path. Rejecting revocation of any candidate
// is conservative: an unused revoked certificate may deny an otherwise valid
// path. No certificate supplied by a peer becomes a trust anchor.
func (i Identity) CandidateCertificateSHA256() []string {
	return append([]string(nil), i.candidateCertificates...)
}

// CandidateSigningKeySHA256 applies the same conservative set to canonical SPKI hashes.
func (i Identity) CandidateSigningKeySHA256() []string {
	return append([]string(nil), i.candidateKeys...)
}

func (i Identity) VerifySignature(message, signature []byte) error {
	if i.key == nil {
		return errors.New("identity has no verified signing key")
	}
	return mldsa.Verify(i.key, message, signature, nil)
}

func parseChain(raw [][]byte) ([]*x509.Certificate, error) {
	if len(raw) == 0 || len(raw) > MaxCertificates {
		return nil, errors.New("certificate count exceeds bound or is empty")
	}
	total := 0
	certs := make([]*x509.Certificate, 0, len(raw))
	for _, der := range raw {
		if len(der) == 0 || len(der) > MaxCertificateBytes-total {
			return nil, errors.New("certificate bytes exceed bound or are empty")
		}
		total += len(der)
		// Own the bytes: x509 retains slices into its input.
		cert, err := x509.ParseCertificate(append([]byte(nil), der...))
		if err != nil {
			return nil, errors.New("invalid certificate DER")
		}
		certs = append(certs, cert)
	}
	return certs, nil
}

func New(cfg Config) (*Verifier, error) {
	tenant, err := uuid.Parse(cfg.Tenant)
	if err != nil || tenant.String() != cfg.Tenant {
		return nil, errors.New("canonical tenant UUID required")
	}
	roots, err := parseChain(cfg.Roots)
	if err != nil {
		return nil, err
	}
	for _, c := range roots {
		// Same eligible-anchor rule as Wendy mesh peerCertificateTrust. An
		// explicitly pinned intermediate may be an anchor; no peer may add one.
		if !c.BasicConstraintsValid || !c.IsCA || c.KeyUsage != 0 && c.KeyUsage&x509.KeyUsageCertSign == 0 {
			return nil, errors.New("configured trust contains a non-CA or non-signing anchor")
		}
	}
	return &Verifier{tenant: cfg.Tenant, roots: roots}, nil
}

func (v *Verifier) Verify(chainDER [][]byte, earliest, latest time.Time) (Identity, error) {
	if v == nil {
		return Identity{}, errors.New("certificate verifier is not configured")
	}
	chain, err := parseChain(chainDER)
	if err != nil {
		return Identity{}, err
	}
	leaf := chain[0]
	validUntil, err := upstreamcerts.VerifyPeerInterval(leaf, chain[1:], v.roots, earliest, latest)
	if err != nil {
		return Identity{}, err
	}
	id, found, err := upstreamcerts.IdentityFromCert(leaf)
	if err != nil || !found || id.TenantUUID != v.tenant || id.EntityType != upstreamcerts.EntityUser {
		return Identity{}, errors.New("certificate does not identify an operator in the configured tenant")
	}
	// Match Wendy meshoperator.ParsePrincipal's canonical human-operator gate.
	p := id.Principal
	if len(p) > 1024 || !utf8.ValidString(p) || p != "spiffe://wendy.sh/tenant/"+id.TenantUUID+"/operator/"+id.EntityID {
		return Identity{}, errors.New("canonical operator principal required")
	}
	u, err := url.Parse(p)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != p {
		return Identity{}, errors.New("operator principal is not canonical URI text")
	}
	for _, c := range p {
		if c < 0x20 || c == 0x7f {
			return Identity{}, errors.New("operator principal contains control characters")
		}
	}
	key, ok := leaf.PublicKey.(*mldsa.PublicKey)
	if !ok || key.Parameters() != mldsa.MLDSA65() {
		return Identity{}, errors.New("operator signing key must be ML-DSA-65")
	}
	certificates, keys := map[string]bool{}, map[string]bool{}
	for _, set := range [][]*x509.Certificate{chain, v.roots} {
		for _, c := range set {
			certificates[digest(c.Raw)] = true
			canonicalSPKI, err := x509.MarshalPKIXPublicKey(c.PublicKey)
			if err != nil {
				return Identity{}, errors.New("candidate public key has no canonical SPKI encoding")
			}
			keys[digest(canonicalSPKI)] = true
		}
	}
	canonicalLeafSPKI, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return Identity{}, errors.New("operator public key has no canonical SPKI encoding")
	}
	return Identity{Principal: p, Tenant: id.TenantUUID, SigningKeySHA256: digest(canonicalLeafSPKI), CertificateSHA256: digest(leaf.Raw), ValidFrom: earliest, ValidUntil: validUntil, key: key, candidateCertificates: sorted(certificates), candidateKeys: sorted(keys)}, nil
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
