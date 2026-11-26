package mtls

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// ClosedServerKeyFile is the TLS server key of a Closed device. It is made
// once and kept, so a client can pin it like an SSH host key.
const ClosedServerKeyFile = "closed-tls-key.pem"

// ClosedServerIdentity loads (or makes, once) the Closed device's ML-DSA-65
// server key and returns a fresh self-signed certificate over it. Clients
// authenticate the device by the key, never by a chain.
func ClosedServerIdentity(configPath string, now time.Time) (certPEM, keyPEM string, err error) {
	path := filepath.Join(configPath, ClosedServerKeyFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if keyPEM, err = certs.GenerateMLDSAKeyPair(); err != nil {
			return "", "", err
		}
		if err = atomicfile.Write(path, []byte(keyPEM), 0o600); err != nil {
			return "", "", fmt.Errorf("saving the closed-device TLS key: %w", err)
		}
	} else if err != nil {
		return "", "", fmt.Errorf("reading the closed-device TLS key: %w", err)
	} else {
		keyPEM = string(data)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", path, err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "wendy-closed-device"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return "", "", fmt.Errorf("self-signing the closed-device certificate: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), keyPEM, nil
}

// NewTrustedKeyTLSConfig is the TLS of a Closed device. A caller proves a key
// by completing the handshake with a client certificate over it; the
// certificate itself is only a carrier (self-signed, any validity), because
// the key is the credential. Keys below minRole are refused at the handshake:
// the registry, which has no per-call check, needs operator.
func NewTrustedKeyTLSConfig(certPEM, keyPEM string, keys *trustedkeys.Store, minRole trustedkeys.Role) (*tls.Config, error) {
	cert, err := certs.TLSKeyPair(certPEM, "", keyPEM)
	if err != nil {
		return nil, fmt.Errorf("loading the closed-device certificate: %w", err)
	}
	if minRole == trustedkeys.RoleNone {
		minRole = trustedkeys.RoleViewer
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
		// A resumed session skips VerifyPeerCertificate; without tickets every
		// connection re-checks the key against the current list.
		SessionTicketsDisabled: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no client certificate: connect with your trusted key")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return fmt.Errorf("reading the client certificate: %w", err)
			}
			role, fp := keys.Lookup(leaf.RawSubjectPublicKeyInfo)
			if role == trustedkeys.RoleNone {
				return fmt.Errorf("key %s is not trusted by this device; ask the device's owner to add it", fp)
			}
			if role < minRole {
				return fmt.Errorf("key %s has the %s role; this needs %s", fp, role, minRole)
			}
			return nil
		},
	}, nil
}
