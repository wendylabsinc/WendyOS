// Package localkey is the CLI's local key: one ML-DSA-65 key pair per CLI
// config directory, generated on first use and never rotated. A Closed device
// trusts it when it closed the device, when `wendy os install --close` seeded
// it, or when an owner added it; the CLI proves it with a certificate it
// self-issues over this key. No cloud login, CA or network is involved.
//
// It is deliberately not the per-session operator key: that one is replaced by
// every login and every certificate renewal, which would lock the owner out of
// every device seeded with it. Losing this key means another owner removes and
// re-adds you, or the device is reinstalled.
package localkey

import (
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// FileName is the key's file in the CLI config directory.
const FileName = "local-operator-key.pem"

// Path is where the key lives. Under sudo it is the invoking user's key:
// sudo resets HOME to root's, where that user's CLI never looks, and a key of
// root's own there would be seeded instead of theirs.
func Path() (string, error) {
	if uid := os.Getenv("SUDO_UID"); geteuid() == 0 && uid != "" && uid != "0" && os.Getenv("WENDY_CONFIG_DIR") == "" {
		u, err := lookupUID(uid)
		if err != nil {
			return "", fmt.Errorf("finding the home of the user who ran sudo (uid %s): %w", uid, err)
		}
		return filepath.Join(u.HomeDir, ".wendy", FileName), nil
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// geteuid and lookupUID are variables so tests can run as root.
var (
	geteuid   = os.Geteuid
	lookupUID = user.LookupId
)

// LoadOrCreate returns the key at path, generating it when absent.
func LoadOrCreate(path string) (crypto.Signer, error) {
	key, err := load(path)
	if !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	// Under sudo a new key would be root's: root-owned in the user's config
	// dir, where their own CLI cannot read it, or under /root when sudo resets
	// HOME, where their CLI never looks. Only the user may create it.
	if uid := os.Getenv("SUDO_UID"); geteuid() == 0 && uid != "" && uid != "0" {
		return nil, fmt.Errorf("%s does not exist and wendy is running under sudo; run wendy without sudo so the key is created as your user", path)
	}
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	// Write a temp file and hard-link it into place: the link fails if another
	// CLI won the race, so a key some device may already trust is never
	// overwritten.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".local-operator-key-*")
	if err != nil {
		return nil, fmt.Errorf("creating local key: %w", err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck
	if _, err := tmp.WriteString(keyPEM); err != nil {
		tmp.Close() //nolint:errcheck
		return nil, fmt.Errorf("writing local key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck
		return nil, fmt.Errorf("writing local key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("writing local key: %w", err)
	}
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("storing local key: %w", err)
	}
	return load(path)
}

func load(path string) (crypto.Signer, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	// Like ssh: a private key others can read is not this operator's alone.
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is accessible by group or others (mode %#o); run chmod 600 on it", path, fi.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := certs.ParseSigningPrivateKeyPEM(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return key, nil
}

// PublicKeyDER is the SubjectPublicKeyInfo DER of key — the form a device's
// trusted-keys file stores and matches.
func PublicKeyDER(key crypto.Signer) ([]byte, error) {
	return x509.MarshalPKIXPublicKey(key.Public())
}

// Fingerprint is the key's "sha256:<hex>" SPKI fingerprint, the form the
// device's trusted-keys file and `wendy device keys` print.
func Fingerprint(key crypto.Signer) (string, error) {
	der, err := PublicKeyDER(key)
	if err != nil {
		return "", err
	}
	return trustedkeys.Fingerprint(der), nil
}

// certLifetime keeps a self-issued certificate short-lived: it is made fresh
// for every connection, so it only has to outlive one session.
const certLifetime = 24 * time.Hour

// Certificate self-issues the client certificate a Closed device's key
// listener accepts (agent mtls.NewTrustedKeyTLSConfig): signed by key itself,
// clientAuth EKU, a short validity window and no tenant identity, so the
// device matches it by key against its trusted keys.
func Certificate(key crypto.Signer, now time.Time) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "wendy-local-operator"},
		// Allow for a device clock a little ahead of ours.
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.Add(certLifetime),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("self-issuing local key certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}
