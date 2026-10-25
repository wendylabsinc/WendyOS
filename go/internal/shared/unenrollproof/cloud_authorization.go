package unenrollproof

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy-go/enrollment"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
)

// VerifyCloudResetDeletion authenticates a reset response and returns the exact
// embedded deletion bytes. A missing GetAsset result is not deletion evidence;
// only the Cloud signature, exact operator request and identity bindings are.
func VerifyCloudResetDeletion(ctx context.Context, token []byte, cloud, principal, fingerprint string, operator *x509.Certificate, earliest, latest time.Time) ([]byte, error) {
	if operator == nil || earliest.IsZero() || latest.Before(earliest) {
		return nil, fmt.Errorf("invalid reset verification bounds")
	}
	deletion, err := resetDeletion(token)
	if err != nil {
		return nil, err
	}
	if _, err := VerifyCloudReset(ctx, token, cloud, principal, fingerprint, deletion, operator, earliest, latest); err != nil {
		return nil, err
	}
	return deletion, nil
}

// resetDeletion extracts untrusted bounded data; it never grants authority.
func resetDeletion(token []byte) ([]byte, error) { return enrollment.ResetDeletion(token) }

// VerifyCloudReset obtains Cloud keys over authenticated HTTPS to the enrolled
// issuer only. No operator credential, caller-provided key or URL is forwarded.
func VerifyCloudReset(ctx context.Context, token []byte, cloud, principal, fingerprint string, deletion []byte, operator *x509.Certificate, earliest, latest time.Time) ([]byte, error) {
	issuer, err := cloudrelay.Issuer(cloud, "")
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{TLSHandshakeTimeout: 5 * time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, Time: func() time.Time { return latest }, VerifyConnection: func(s tls.ConnectionState) error {
		for _, chain := range s.VerifiedChains {
			for _, cert := range chain {
				if earliest.Before(cert.NotBefore) || latest.After(cert.NotAfter) {
					return fmt.Errorf("Cloud HTTPS chain not valid throughout authenticated interval")
				}
			}
		}
		return nil
	}}}
	defer transport.CloseIdleConnections()
	verifier := cloudrelay.Verifier{Issuer: issuer, HTTP: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	_, key, err := verifier.VerifyResetSignature(ctx, string(token))
	if err != nil {
		return nil, err
	}
	if err = VerifySavedCloudReset(token, key, cloud, principal, fingerprint, deletion, operator, earliest, latest); err != nil {
		return nil, err
	}
	return key, nil
}

// VerifySavedCloudReset delegates canonical proof, identity and freshness checks.
// Raw Cloud keys remain trusted only from protected admission evidence or HTTPS.
func VerifySavedCloudReset(token, key []byte, cloud, principal, fingerprint string, deletion []byte, operator *x509.Certificate, earliest, latest time.Time) error {
	if operator == nil {
		return fmt.Errorf("invalid reset verification bounds")
	}
	return enrollment.VerifySavedCloudReset(context.Background(), receiptVerifier{}, token, key, cloud, principal, fingerprint, deletion, operator.Raw, earliest, latest)
}
