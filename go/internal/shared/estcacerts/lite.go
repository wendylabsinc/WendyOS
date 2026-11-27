package estcacerts

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

const Classical = "ECDSA_P256"

// Fetch retrieves the bounded, public certificate set over authenticated HTTPS.
func Fetch(ctx context.Context, client *http.Client, endpoint *url.URL) ([]*x509.Certificate, string, error) {
	if endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return nil, "", fmt.Errorf("invalid EST CA endpoint")
	}
	secure := *client
	secure.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := secure.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.TLS == nil || len(resp.TLS.VerifiedChains) == 0 {
		return nil, "", fmt.Errorf("EST CA discovery failed (HTTP %d)", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128<<10|1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > 128<<10 {
		return nil, "", fmt.Errorf("EST CA response exceeds 128 KiB")
	}
	result, err := ParseCertificates(body, resp.Header.Get("Content-Type"))
	return result, resp.Header.Get("X-Wendy-Chain-Algorithm"), err
}

func IsLiteDeviceCA(c *x509.Certificate, tenant string) bool {
	key, ok := c.PublicKey.(*ecdsa.PublicKey)
	return ok && key.Curve.Params().BitSize == 256 && c.IsCA && c.MaxPathLenZero &&
		c.Subject.CommonName == tenant+" Lite Device CA"
}

// LitePath uses the existing platform anchors and returns only untrusted
// intermediate candidates, with the validity window of the selected path.
func LitePath(bundle []*x509.Certificate, trustPEM, tenant string, now time.Time) ([]*x509.Certificate, certs.CertificateWindow, error) {
	return litePath(bundle, trustPEM, tenant, now, false)
}

// LitePathForEnrolledPeer uses CA material from the existing device enrollment.
// ACME may omit the self-signed root. In that case, verify up to an already
// trusted CA in the stored enrollment chain, never to one received over EST.
func LitePathForEnrolledPeer(bundle []*x509.Certificate, trustPEM, tenant string, now time.Time) ([]*x509.Certificate, certs.CertificateWindow, error) {
	return litePath(bundle, trustPEM, tenant, now, true)
}

func litePath(bundle []*x509.Certificate, trustPEM, tenant string, now time.Time, useEnrolledAnchors bool) ([]*x509.Certificate, certs.CertificateWindow, error) {
	anchors, err := certs.ParseCertsFromPEM([]byte(trustPEM))
	if err != nil {
		return nil, certs.CertificateWindow{}, err
	}
	roots, enrolled, intermediates := x509.NewCertPool(), x509.NewCertPool(), x509.NewCertPool()
	for _, ca := range anchors {
		if !ca.BasicConstraintsValid || !ca.IsCA || ca.KeyUsage != 0 && ca.KeyUsage&x509.KeyUsageCertSign == 0 ||
			IsLiteDeviceCA(ca, tenant) {
			continue
		}
		if bytes.Equal(ca.RawIssuer, ca.RawSubject) && ca.CheckSignatureFrom(ca) == nil {
			roots.AddCert(ca)
		}
		if useEnrolledAnchors {
			enrolled.AddCert(ca)
		}
	}
	for _, ca := range bundle {
		if ca.IsCA && !bytes.Equal(ca.RawIssuer, ca.RawSubject) {
			intermediates.AddCert(ca)
		}
	}
	for _, ca := range bundle {
		if !IsLiteDeviceCA(ca, tenant) || bytes.Equal(ca.RawIssuer, ca.RawSubject) {
			continue
		}
		options := x509.VerifyOptions{Roots: roots, Intermediates: intermediates,
			CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}
		paths, err := ca.Verify(options)
		if err != nil && useEnrolledAnchors {
			options.Roots = enrolled
			paths, err = ca.Verify(options)
		}
		if err != nil || len(paths) == 0 || len(paths[0]) < 2 {
			continue
		}
		path := paths[0]
		window := certs.CertificateWindow{NotBefore: ca.NotBefore, NotAfter: ca.NotAfter}
		for _, parent := range path[1:] {
			window = window.Intersect(certs.CertificateWindow{NotBefore: parent.NotBefore, NotAfter: parent.NotAfter})
		}
		return path[:len(path)-1], window, nil
	}
	return nil, certs.CertificateWindow{}, fmt.Errorf("no parent-signed tenant Lite CA path to the platform trust")
}

func PEM(certificates []*x509.Certificate) string {
	var out bytes.Buffer
	for _, cert := range certificates {
		_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	}
	return out.String()
}
