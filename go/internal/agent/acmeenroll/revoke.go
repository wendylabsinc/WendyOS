package acmeenroll

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"golang.org/x/crypto/acme"
)

// CertificateIdentity verifies the installed leaf belongs to this exact ACME
// deployment/device. Returned values are public; no account/device keys escape.
func CertificateIdentity(cfg Config, certificate string) (principal, fingerprint, serial string, der []byte, err error) {
	principal, err = cfg.PrincipalURI()
	if err != nil {
		return
	}
	block, _ := pem.Decode([]byte(certificate))
	if block == nil || block.Type != "CERTIFICATE" {
		err = errors.New("installed certificate is invalid")
		return
	}
	leaf, e := x509.ParseCertificate(block.Bytes)
	if e != nil {
		err = errors.New("installed certificate cannot be parsed")
		return
	}
	actual, ok := certs.TenantPrincipalFromCert(leaf)
	if !ok || actual != principal {
		err = errors.New("installed certificate does not match the ACME identity")
		return
	}
	sum := sha256.Sum256(block.Bytes)
	return principal, hex.EncodeToString(sum[:]), leaf.SerialNumber.Text(16), block.Bytes, nil
}

// Revoke revokes only the installed leaf. Never creates/registers/rotates a key
// or requests EAB. onlyReturnExisting resolves the account that already owns it.
func Revoke(ctx context.Context, cfg Config, accountKeyPath, certificate string) error {
	_, _, _, der, err := CertificateIdentity(cfg, certificate)
	if err != nil {
		return err
	}
	path := scopedAccountKeyPath(accountKeyPath, cfg)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		data, err = os.ReadFile(accountKeyPath)
	}
	if err != nil {
		return errors.New("existing ACME account key unavailable; no key was created or rotated")
	}
	defer clear(data)
	key, err := parseECPrivateKeyPEM(data)
	if err != nil {
		return errors.New("existing ACME account key is invalid")
	}
	client := &acme.Client{Key: key, DirectoryURL: cfg.DirectoryURL, HTTPClient: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	directory, err := client.Discover(ctx)
	if err != nil {
		return fmt.Errorf("ACME discovery failed")
	}
	// Do not sign a revocation at an unrelated origin/tenant supplied in a
	// directory response. The PKI deployment exposes this exact scoped route.
	base := strings.TrimSuffix(cfg.DirectoryURL, "/directory")
	if directory.RevokeURL != base+"/revoke-cert" || directory.RegURL != base+"/new-account" || directory.NonceURL != base+"/new-nonce" {
		return errors.New("ACME revocation endpoint does not match the enrolled tenant")
	}
	account, err := client.GetReg(ctx, "")
	if err != nil || account == nil || !strings.HasPrefix(account.URI, base+"/acct/") {
		return errors.New("existing ACME account lookup failed; no account was registered")
	}
	client.KID = acme.KeyID(account.URI)
	err = client.RevokeCert(ctx, nil, der, acme.CRLReasonCessationOfOperation)
	var problem *acme.Error
	if errors.As(err, &problem) && problem.ProblemType == "urn:ietf:params:acme:error:alreadyRevoked" {
		return nil
	}
	if err != nil {
		return errors.New("ACME certificate revocation failed; enrollment and keys retained")
	}
	return nil
}
