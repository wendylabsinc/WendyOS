package liteenroll

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	litepb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

type testInspector struct{}

func (testInspector) InspectCertificate(der []byte) (*platform.LiteCertificate, error) {
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	var outer struct {
		TBS       asn1.RawValue
		Algorithm pkix.AlgorithmIdentifier
		Signature asn1.BitString
	}
	if _, err = asn1.Unmarshal(der, &outer); err != nil {
		return nil, err
	}
	key, ok := c.PublicKey.(*ecdsa.PublicKey)
	return &platform.LiteCertificate{Raw: c.Raw, RawIssuer: c.RawIssuer, RawSubject: c.RawSubject, IsCA: c.IsCA, BasicConstraintsValid: c.BasicConstraintsValid, NotBefore: c.NotBefore, NotAfter: c.NotAfter, PublicKeyAlgorithm: c.PublicKeyAlgorithm.String(), SignatureAlgorithm: c.SignatureAlgorithm.String(), SignatureOID: outer.Algorithm.Algorithm.String(), P256: ok && key.Curve == elliptic.P256(), SelfSignatureValid: c.CheckSignatureFrom(c) == nil}, nil
}
func testProfile() TrustProfile { return TrustProfile{Inspector: testInspector{}} }
func testTrustProfile(c *litepb.WendyComEnrollmentChallenge) TrustProfile {
	return TrustProfileFromChallenge(c, testInspector{})
}
func validateMbedTLSTrustBundle(data []byte) error { return ValidateTrustBundle(data, testProfile()) }

type testHTTP struct{ client *http.Client }

func (h testHTTP) DoLite(ctx context.Context, request platform.HTTPRequest) (*platform.LiteHTTPResponse, error) {
	req, err := http.NewRequestWithContext(ctx, request.Method, request.URL, bytes.NewReader(request.Body))
	if err != nil {
		return nil, err
	}
	req.Header = http.Header(request.Headers)
	c := *h.client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	out := &platform.LiteHTTPResponse{HTTPResponse: platform.HTTPResponse{StatusCode: resp.StatusCode, Headers: platform.HTTPHeaders(resp.Header), Body: resp.Body}}
	if resp.TLS != nil {
		for _, chain := range resp.TLS.VerifiedChains {
			var certs []*platform.LiteCertificate
			for _, c := range chain {
				cert, _ := testInspector{}.InspectCertificate(c.Raw)
				certs = append(certs, cert)
			}
			out.VerifiedChains = append(out.VerifiedChains, certs)
		}
	}
	return out, nil
}
func discoverTrust(ctx context.Context, c *http.Client, cfg *litepb.WendyConfEnrollment, endpoint string) error {
	return DiscoverTrustForProfile(ctx, testHTTP{c}, cfg, endpoint, testProfile())
}
func discoverTrustForProfile(ctx context.Context, c *http.Client, cfg *litepb.WendyConfEnrollment, endpoint string, p TrustProfile) error {
	return DiscoverTrustForProfile(ctx, testHTTP{c}, cfg, endpoint, p)
}
func ProvisionTrust(cfg *litepb.WendyConfEnrollment, d, t, h string) (*http.Client, error) {
	return ProvisionTrustForProfile(cfg, d, t, h, testProfile())
}
func ProvisionTrustForProfile(cfg *litepb.WendyConfEnrollment, d, t, h string, p TrustProfile) (*http.Client, error) {
	if d == "" && t == "" && h == "" {
		return http.DefaultClient, nil
	}
	if d == "" || h == "" || (cfg.TimeUrl != "roughtime" && cfg.TimeUrl != DefaultTimeURL && t == "") {
		return nil, fmt.Errorf("missing paths")
	}
	bundles := make([][]byte, 3)
	for i, path := range []string{d, t, h} {
		if path == "" {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		bundles[i], err = io.ReadAll(io.LimitReader(f, maxTrustBundle+1))
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	if err := ProvisionTrustBundles(cfg, bundles[0], bundles[1], bundles[2], p); err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cfg.HttpsRoots) {
		return nil, fmt.Errorf("no supported roots")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	return &http.Client{Transport: transport, Timeout: 15 * time.Second}, nil
}

const tenant = "11111111-1111-4111-8111-111111111111"

func checkTrustSupport(cfg *litepb.WendyConfEnrollment, ch *litepb.WendyComEnrollmentChallenge) error {
	return CheckTrustSupport(cfg, ch, testInspector{})
}
