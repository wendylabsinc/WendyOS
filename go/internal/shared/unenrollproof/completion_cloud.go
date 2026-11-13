package unenrollproof

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
)

// VerifyCompletionCloud re-establishes Cloud key trust for a REMOTE receipt.
// A device signature alone never makes its supplied CloudVerificationKey trusted.
// TLS is checked at present time; historical proof is checked at admission time.
func VerifyCompletionCloud(ctx context.Context, record *Completion) error {
	if record == nil || record.Version != 2 {
		return fmt.Errorf("authenticated Cloud receipt required")
	}
	issuer, err := cloudrelay.Issuer(record.Cloud, "")
	if err != nil {
		return err
	}
	transport := &http.Transport{TLSHandshakeTimeout: 5 * time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13}}
	defer transport.CloseIdleConnections()
	v := cloudrelay.Verifier{Issuer: issuer, HTTP: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	_, key, err := v.VerifyResetSignature(ctx, string(record.CloudAuthorization))
	if err != nil {
		return err
	}
	operator, err := x509.ParseCertificate(record.ResetOperator)
	if err != nil {
		return err
	}
	at := record.AuthorizationTime()
	return VerifySavedCloudReset(record.CloudAuthorization, key, record.Cloud, record.Principal, record.Fingerprint, record.CloudDeletion, operator, at, at)
}
