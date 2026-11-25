package wendycert

import (
	"crypto/mldsa"
	"crypto/x509"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/world/production/internal/upstreamcerts"
)

// DeviceVerifier accepts only canonical enrolled-device UUID principals. It is
// deliberately separate from Verifier: a device can never become an operator
// identity by being accepted here. Enrollment establishes no site permission.
type DeviceVerifier struct{ trust *Verifier }

type DeviceIdentity struct {
	Principal, Tenant, SigningKeySHA256, CertificateSHA256 string
	ValidFrom, ValidUntil                                  time.Time
	key                                                    *mldsa.PublicKey
	candidateCertificates, candidateKeys                   []string
}

func NewDevice(cfg Config) (*DeviceVerifier, error) {
	v, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return &DeviceVerifier{trust: v}, nil
}

// Verify requires a single certificate path valid throughout an authenticated
// physical interval. It has no unknown-boot-time or wall-clock allowance.
func (v *DeviceVerifier) Verify(chainDER [][]byte, earliest, latest time.Time) (DeviceIdentity, error) {
	if v == nil || v.trust == nil {
		return DeviceIdentity{}, errors.New("device certificate verifier is not configured")
	}
	chain, err := parseChain(chainDER)
	if err != nil {
		return DeviceIdentity{}, err
	}
	leaf := chain[0]
	until, err := upstreamcerts.VerifyPeerInterval(leaf, chain[1:], v.trust.roots, earliest, latest)
	if err != nil {
		return DeviceIdentity{}, err
	}
	id, found, err := upstreamcerts.IdentityFromCert(leaf)
	device, uuidErr := uuid.Parse(id.EntityID)
	if err != nil || !found || uuidErr != nil || device.String() != id.EntityID || id.EntityType != upstreamcerts.EntityAsset || id.TenantUUID != v.trust.tenant || id.Principal != "spiffe://wendy.sh/tenant/"+id.TenantUUID+"/device/"+id.EntityID {
		return DeviceIdentity{}, errors.New("canonical device UUID principal in the configured tenant required")
	}
	key, ok := leaf.PublicKey.(*mldsa.PublicKey)
	if !ok || key.Parameters() != mldsa.MLDSA65() {
		return DeviceIdentity{}, errors.New("device signing key must be ML-DSA-65")
	}
	certificates, keys := map[string]bool{}, map[string]bool{}
	for _, set := range [][]*x509.Certificate{chain, v.trust.roots} {
		for _, cert := range set {
			certificates[digest(cert.Raw)] = true
			spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
			if err != nil {
				return DeviceIdentity{}, errors.New("candidate key has no canonical SPKI encoding")
			}
			keys[digest(spki)] = true
		}
	}
	spki, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return DeviceIdentity{}, errors.New("device key has no canonical SPKI encoding")
	}
	return DeviceIdentity{Principal: id.Principal, Tenant: id.TenantUUID, SigningKeySHA256: digest(spki), CertificateSHA256: digest(leaf.Raw), ValidFrom: earliest, ValidUntil: until, key: key, candidateCertificates: sorted(certificates), candidateKeys: sorted(keys)}, nil
}

func (i DeviceIdentity) CandidateCertificateSHA256() []string {
	return append([]string(nil), i.candidateCertificates...)
}
func (i DeviceIdentity) CandidateSigningKeySHA256() []string {
	return append([]string(nil), i.candidateKeys...)
}
func (i DeviceIdentity) VerifySignature(message, signature []byte) error {
	if i.key == nil {
		return errors.New("device identity has no verified signing key")
	}
	return mldsa.Verify(i.key, message, signature, nil)
}
