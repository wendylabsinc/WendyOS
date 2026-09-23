package localmesh

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// Credentials reuses the agent's provisioned signer and ML-DSA-aware trust
// verifier. Reconstruct this object (and the session/cache) when provisioning or
// trust changes. No mesh-specific private key or trust-on-first-use is created.
type Credentials struct {
	Org, Asset  int32
	Certificate tls.Certificate
	Signer      crypto.Signer
	Verify      IdentityVerifier
	peerTLS     func(int32) (*tls.Config, error)
}

func NewCredentials(org, asset int32, certPEM, chainPEM, keyPEM string) (*Credentials, error) {
	if _, _, err := Addresses(org, asset); err != nil {
		return nil, err
	}
	base, err := mtls.NewClientTLSConfigExpectingPeer(certPEM, chainPEM, keyPEM, nil, strconv.Itoa(int(asset)))
	if err != nil {
		return nil, err
	}
	if len(base.Certificates) != 1 {
		return nil, errors.New("missing local-mesh identity")
	}
	cert := base.Certificates[0]
	if len(cert.Certificate) == 0 {
		return nil, errors.New("missing local-mesh leaf certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	self, found, err := certs.IdentityFromCert(leaf)
	if err != nil || !found || self.EntityType != certs.EntityAsset || self.OrgID != org || self.EntityID != strconv.Itoa(int(asset)) {
		return nil, errors.New("local-mesh identity does not match enrolled org and asset")
	}
	if err = base.VerifyPeerCertificate(cert.Certificate, nil); err != nil {
		return nil, err
	}
	signer, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("identity key cannot sign")
	}
	verifyChain, err := mtls.NewClientTLSConfig(certPEM, chainPEM, keyPEM, nil)
	if err != nil {
		return nil, err
	}
	trustChain, err := certs.ParseCertsFromPEM([]byte(chainPEM))
	if err != nil {
		return nil, err
	}
	verify := func(chain [][]byte, now time.Time) (Identity, error) {
		if len(chain) == 0 || len(chain) > 16 {
			return Identity{}, errors.New("invalid identity bundle")
		}
		if err := verifyChain.VerifyPeerCertificate(chain, nil); err != nil {
			return Identity{}, err
		}
		leaf, err := x509.ParseCertificate(chain[0])
		if err != nil {
			return Identity{}, err
		}
		id, found, err := certs.IdentityFromCert(leaf)
		if err != nil {
			return Identity{}, err
		}
		if !found || id.EntityType != "asset" || id.OrgID != org || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
			return Identity{}, errors.New("invalid local-mesh origin")
		}
		peer, err := strconv.ParseInt(id.EntityID, 10, 32)
		if err != nil {
			return Identity{}, err
		}
		if _, _, err = Addresses(org, int32(peer)); err != nil {
			return Identity{}, err
		}
		expires := leaf.NotAfter
		for _, issuer := range trustChain {
			if issuer.NotAfter.Before(expires) {
				expires = issuer.NotAfter
			}
		}
		return Identity{org, int32(peer), expires}, nil
	}
	return &Credentials{Org: org, Asset: asset, Certificate: cert, Signer: signer, Verify: verify, peerTLS: func(peer int32) (*tls.Config, error) {
		c, err := mtls.NewClientTLSConfigExpectingPeer(certPEM, chainPEM, keyPEM, nil, strconv.Itoa(int(peer)))
		if err != nil {
			return nil, err
		}
		c.MinVersion = tls.VersionTLS13
		c.MaxVersion = tls.VersionTLS13
		c.NextProtos = []string{LinkALPN}
		c.ClientAuth = tls.RequireAnyClientCert
		c.ClientSessionCache = nil
		c.SessionTicketsDisabled = true
		return c, nil
	}}, nil
}

func (c *Credentials) PeerTLS(peer int32) (*tls.Config, error) {
	if peer == c.Asset {
		return nil, errors.New("self link")
	}
	if _, _, err := Addresses(c.Org, peer); err != nil {
		return nil, err
	}
	return c.peerTLS(peer)
}
