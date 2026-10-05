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

// RevocationError exposes only the phase, HTTP status and standardized ACME
// problem type. Never return backend details, URLs, nonces or key material.
type RevocationError struct {
	Phase       string
	HTTPStatus  int
	ProblemType string
}

func (e *RevocationError) Error() string {
	return fmt.Sprintf("ACME %s failed (HTTP %d, problem %s); enrollment and keys retained", e.Phase, e.HTTPStatus, e.ProblemType)
}

func revocationError(phase string, err error) error {
	diagnostic := &RevocationError{Phase: phase, ProblemType: "none"}
	var problem *acme.Error
	if errors.As(err, &problem) {
		diagnostic.HTTPStatus = problem.StatusCode
		// Backend-provided text must not become a credential/log channel.
		for _, suffix := range []string{"externalAccountRequired", "accountDoesNotExist", "unauthorized", "malformed", "serverInternal", "badNonce", "alreadyRevoked", "badRevocationReason"} {
			if problem.ProblemType == "urn:ietf:params:acme:error:"+suffix {
				diagnostic.ProblemType = suffix
				break
			}
		}
		if diagnostic.ProblemType == "none" {
			diagnostic.ProblemType = "other"
		}
	}
	return diagnostic
}

// CheckRevocationAccount checks directory/account lookup only. It never sends
// revokeCert, registers an account, creates a key, or requests EAB.
func CheckRevocationAccount(ctx context.Context, cfg Config, accountKeyPath, certificate string) error {
	_, err := existingRevocationClient(ctx, cfg, accountKeyPath, certificate)
	return err
}

// Revoke revokes only the installed leaf. Never creates/registers/rotates a key
// or requests EAB. onlyReturnExisting resolves the account that already owns it.
func Revoke(ctx context.Context, cfg Config, accountKeyPath, certificate string) error {
	client, err := existingRevocationClient(ctx, cfg, accountKeyPath, certificate)
	if err != nil {
		return err
	}
	_, _, _, der, err := CertificateIdentity(cfg, certificate)
	if err != nil {
		return err
	}
	err = client.RevokeCert(ctx, nil, der, acme.CRLReasonCessationOfOperation)
	var problem *acme.Error
	if errors.As(err, &problem) && problem.ProblemType == "urn:ietf:params:acme:error:alreadyRevoked" {
		return nil
	}
	if err != nil {
		return revocationError("revoke_certificate", err)
	}
	return nil
}

func existingRevocationClient(ctx context.Context, cfg Config, accountKeyPath, certificate string) (*acme.Client, error) {
	_, _, _, _, err := CertificateIdentity(cfg, certificate)
	if err != nil {
		return nil, revocationError("certificate_binding", err)
	}
	path := scopedAccountKeyPath(accountKeyPath, cfg)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		data, err = os.ReadFile(accountKeyPath)
	}
	if err != nil {
		return nil, revocationError("account_key_read", err)
	}
	defer clear(data)
	key, err := parseECPrivateKeyPEM(data)
	if err != nil {
		return nil, revocationError("account_key_parse", err)
	}
	client := &acme.Client{Key: key, DirectoryURL: cfg.DirectoryURL, HTTPClient: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	directory, err := client.Discover(ctx)
	if err != nil {
		return nil, revocationError("directory_discovery", err)
	}
	// Do not sign a revocation at an unrelated origin/tenant supplied in a
	// directory response. The PKI deployment exposes this exact scoped route.
	base := strings.TrimSuffix(cfg.DirectoryURL, "/directory")
	if directory.RevokeURL != base+"/revoke-cert" || directory.RegURL != base+"/new-account" || directory.NonceURL != base+"/new-nonce" {
		return nil, revocationError("directory_binding", nil)
	}
	account, err := client.GetReg(ctx, "")
	if err != nil {
		return nil, revocationError("existing_account_lookup", err)
	}
	if account == nil || !strings.HasPrefix(account.URI, base+"/acct/") {
		return nil, revocationError("account_binding", nil)
	}
	client.KID = acme.KeyID(account.URI)
	return client, nil
}
