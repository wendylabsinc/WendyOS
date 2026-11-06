package backend

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

const MaxSharedLoginBytes = 64 * 1024

type sharedLogin struct {
	Version   int               `json:"version"`
	CreatedAt time.Time         `json:"createdAt"`
	ExpiresAt time.Time         `json:"expiresAt"`
	Account   config.AuthConfig `json:"account"`
}

// ExportSharedLogin is called only by the explicit Share login control. Never
// share rotating refresh tokens, headless service credentials, or registry keys
// whose cloud registration belongs to one device.
func (b *Backend) ExportSharedLogin() ([]byte, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	auth, err := config.ResolveAuth(cfg, "", nil)
	if err != nil {
		return nil, err
	}
	if auth.ServiceAccount != "" || strings.HasPrefix(auth.Name, "icloud-") {
		return nil, errors.New("share a login created on this device, not an imported or service-account session")
	}
	snapshot, err := makeSharedLogin(*auth, time.Now())
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(snapshot)
	if len(data) > MaxSharedLoginBytes {
		return nil, errors.New("this account has too many credentials to share through Keychain")
	}
	return data, err
}

func makeSharedLogin(auth config.AuthConfig, now time.Time) (sharedLogin, error) {
	// Construct an allowlist rather than copying AuthConfig, so future secrets
	// added to config cannot accidentally become synchronizable.
	out := config.AuthConfig{Name: auth.Name, CloudDashboard: auth.CloudDashboard, CloudGRPC: auth.CloudGRPC,
		OAuthIssuer: auth.OAuthIssuer, OAuthClientID: auth.OAuthClientID, OAuthResource: auth.OAuthResource,
		PKIEndpoint: auth.PKIEndpoint, PKIResource: auth.PKIResource}
	expiry := now.Add(24 * time.Hour)
	for _, certificate := range auth.Certificates {
		key, err := certificate.PrivateKeyPEM()
		if err != nil {
			return sharedLogin{}, err
		}
		certificate.PemPrivateKey = key
		pair, err := certs.TLSKeyPair(certificate.PemCertificate, certificate.PemCertificateChain, key)
		if err != nil {
			return sharedLogin{}, errors.New("could not validate the account certificate")
		}
		leafs, err := certs.ParseCertsFromPEM([]byte(certificate.PemCertificate))
		if err != nil || len(leafs) != 1 || len(pair.Certificate) == 0 {
			return sharedLogin{}, errors.New("invalid account certificate")
		}
		if now.Before(leafs[0].NotBefore) || !now.Before(leafs[0].NotAfter) {
			continue
		}
		if leafs[0].NotAfter.Before(expiry) {
			expiry = leafs[0].NotAfter
		}
		out.Certificates = append(out.Certificates, certificate)
	}
	if len(out.Certificates) == 0 {
		return sharedLogin{}, errors.New("sign in on this device to obtain a current device-access certificate before sharing")
	}
	// OAuth tokens have an explicit expiry. Unbounded legacy API keys are not
	// copied. Sharing the associated DPoP key preserves the token's binding.
	if tokenExpiry, err := time.Parse(time.RFC3339, auth.OAuthExpiresAt); err == nil && tokenExpiry.After(now.Add(time.Minute)) {
		out.APIKey, err = auth.BearerToken()
		if err != nil {
			return sharedLogin{}, err
		}
		if out.APIKey != "" {
			out.DPoPPrivateKey, err = auth.OAuthDPoPKey()
			if err != nil {
				return sharedLogin{}, err
			}
			if tokenExpiry.Before(expiry) {
				expiry = tokenExpiry
			}
		}
	}
	// The imported context is explicitly a non-refreshable credential snapshot.
	out.OAuthExpiresAt = expiry.UTC().Format(time.RFC3339)
	return sharedLogin{Version: 1, CreatedAt: now.UTC(), ExpiresAt: expiry.UTC(), Account: out}, nil
}

func parseSharedLogin(data []byte, now time.Time) (sharedLogin, error) {
	var value sharedLogin
	if len(data) == 0 || len(data) > MaxSharedLoginBytes {
		return value, errors.New("invalid shared login size")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&value); err != nil {
		return value, errors.New("invalid shared login")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return value, errors.New("invalid trailing shared login data")
	}
	a := value.Account
	if value.Version != 1 || value.CreatedAt.After(now.Add(time.Minute)) || !value.ExpiresAt.After(now) || value.ExpiresAt.Sub(value.CreatedAt) > 24*time.Hour || !value.ExpiresAt.After(value.CreatedAt) {
		return value, errors.New("shared login has expired; share a current login from your other device")
	}
	if a.RefreshToken != "" || a.RegistryPrivateKey != "" || a.RegistryKeyID != "" || a.ServiceAccount != "" || a.LitePrivateKey != "" || a.LiteCertificate != "" || len(a.Certificates) == 0 || len(a.Certificates) > 16 || a.CloudGRPC == "" || strings.ContainsAny(a.CloudGRPC, "\r\n\x00") {
		return value, errors.New("unsupported shared credential fields")
	}
	// Reject machine-local Keychain references and mismatched certificate/key
	// pairs. Real server trust checks still happen during every connection.
	for _, c := range a.Certificates {
		if strings.HasPrefix(c.PemPrivateKey, "keychain:") {
			return value, errors.New("shared login contains a device-local reference")
		}
		if _, err := certs.TLSKeyPair(c.PemCertificate, c.PemCertificateChain, c.PemPrivateKey); err != nil {
			return value, errors.New("shared certificate and private key do not match")
		}
		leaves, err := certs.ParseCertsFromPEM([]byte(c.PemCertificate))
		if err != nil || len(leaves) != 1 || now.Before(leaves[0].NotBefore) || value.ExpiresAt.After(leaves[0].NotAfter) {
			return value, errors.New("shared certificate validity does not cover this login")
		}
	}
	for _, secret := range []string{a.APIKey, a.DPoPPrivateKey} {
		if strings.HasPrefix(secret, "keychain:") {
			return value, errors.New("shared login contains a device-local reference")
		}
	}
	expiry, err := time.Parse(time.RFC3339, a.OAuthExpiresAt)
	if err != nil || expiry.After(value.ExpiresAt) {
		return value, errors.New("invalid shared token expiry")
	}
	return value, nil
}

func (b *Backend) ImportSharedLogin(data []byte) error {
	value, err := parseSharedLogin(data, time.Now())
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil || b.lite != nil {
		return errors.New("disconnect from the current device before using a shared login")
	}
	sum := sha256.Sum256([]byte(value.Account.CloudGRPC))
	name := "icloud-" + hex.EncodeToString(sum[:6])
	return config.Update(func(cfg *config.Config) (bool, error) {
		index := -1
		for i, existing := range cfg.Auth {
			if existing.CloudGRPC != value.Account.CloudGRPC {
				continue
			}
			if existing.Name != name || existing.RefreshToken != "" || existing.ServiceAccount != "" {
				return false, fmt.Errorf("an account for this cloud already exists on this device; use or sign in to that account")
			}
			index = i
		}
		value.Account.Name = name
		if index >= 0 {
			cfg.Auth[index] = value.Account
		} else {
			cfg.Auth = append(cfg.Auth, value.Account)
		}
		cfg.CurrentContext = name
		return true, nil
	})
}
