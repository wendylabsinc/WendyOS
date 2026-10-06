package liteenroll

import (
	"bytes"
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

// DiscoverTrust bootstraps through the enrolling computer's HTTPS trust store.
// Only authenticated CA-discovery content and verified TLS trust anchors are
// provisioned; unverified peer certificates never become roots.
func DiscoverTrust(ctx context.Context, cfg *litepb.WendyConfEnrollment, endpoint string) (*http.Client, error) {
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("CA discovery redirects are not allowed")
	}}
	if err := discoverTrust(ctx, client, cfg, endpoint); err != nil {
		return nil, err
	}
	return client, nil
}

func discoverTrust(ctx context.Context, client *http.Client, cfg *litepb.WendyConfEnrollment, endpoint string) error {
	if cfg.TimeUrl != "roughtime" {
		return fmt.Errorf("RFC 3161 enrollment requires explicit --device-roots, --https-roots and --tsa-roots")
	}
	if endpoint == "" {
		csr, err := url.Parse(cfg.CsrUrl)
		if err != nil {
			return err
		}
		suffix, ok := strings.CutPrefix(csr.Host, "csr.")
		if !ok || suffix == "" {
			return fmt.Errorf("pass --ca-certs-url for this PKI deployment, or supply --device-roots and --https-roots")
		}
		endpoint = "https://est." + suffix + "/.well-known/est/" + cfg.TenantId + "/cacerts"
	}
	var deviceRoots []byte
	httpsRoots := []byte{}
	seen := map[string]bool{}
	for i, target := range []string{endpoint, cfg.CsrUrl} {
		u, err := url.Parse(target)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("CA discovery requires an HTTPS URL without credentials or fragment")
		}
		method := http.MethodGet
		if i == 1 {
			method = http.MethodHead
		}
		req, err := http.NewRequestWithContext(ctx, method, target, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("discovering PKI trust from %s: %w", u.Host, err)
		}
		if resp.TLS == nil || len(resp.TLS.VerifiedChains) == 0 {
			resp.Body.Close()
			return fmt.Errorf("CA discovery HTTPS connection was not verified")
		}
		for _, chain := range resp.TLS.VerifiedChains {
			if len(chain) == 0 {
				continue
			}
			root := chain[len(chain)-1]
			if !root.IsCA {
				resp.Body.Close()
				return fmt.Errorf("HTTPS chain does not terminate at a CA")
			}
			if !seen[string(root.Raw)] {
				seen[string(root.Raw)] = true
				httpsRoots = append(httpsRoots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})...)
			}
		}
		if i == 0 {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 128*1024+1))
			resp.Body.Close()
			if readErr != nil {
				return readErr
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("CA discovery returned HTTP %d", resp.StatusCode)
			}
			if len(body) > 128*1024 {
				return fmt.Errorf("CA discovery response too large")
			}
			deviceRoots, err = discoveryRoots(body, resp.Header.Get("Content-Type"))
			if err != nil {
				return err
			}
		} else {
			resp.Body.Close()
		}
	}
	if err := validateTrustBundle(httpsRoots); err != nil {
		return fmt.Errorf("discovered HTTPS roots: %w", err)
	}
	cfg.DeviceRoots, cfg.HttpsRoots = deviceRoots, httpsRoots
	cfg.ProvisionTrust = true
	return nil
}

// EST responses contain untrusted intermediates as well as roots. Install only
// self-issued CAs from the authenticated discovery response.
func discoveryRoots(body []byte, contentType string) ([]byte, error) {
	var certs []*x509.Certificate
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("-----BEGIN CERTIFICATE-----")) {
		if err := validateTrustBundle(body); err != nil {
			return nil, err
		}
		for rest := body; len(bytes.TrimSpace(rest)) > 0; {
			block, tail := pem.Decode(rest)
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, err
			}
			certs = append(certs, cert)
			rest = tail
		}
	} else {
		media, _, err := mime.ParseMediaType(contentType)
		if err != nil || media != "application/pkcs7-mime" {
			return nil, fmt.Errorf("CA discovery expected PEM or EST certs-only CMS")
		}
		der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
		if err != nil {
			return nil, fmt.Errorf("invalid EST base64: %w", err)
		}
		var ci struct {
			Type    asn1.ObjectIdentifier
			Content asn1.RawValue
		}
		rest, err := asn1.Unmarshal(der, &ci)
		if err != nil || len(rest) != 0 || !ci.Type.Equal(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}) {
			return nil, fmt.Errorf("invalid EST SignedData")
		}
		sdDER := ci.Content.FullBytes
		// Accept RFC 5652 explicit content and the direct SEQUENCE emitted by
		// existing pki-core deployments.
		if ci.Content.Class == 2 && ci.Content.Tag == 0 {
			sdDER = ci.Content.Bytes
		}
		var sd struct {
			Version int
			Digests []pkix.AlgorithmIdentifier `asn1:"set"`
			Content struct{ Type asn1.ObjectIdentifier }
			Certs   []asn1.RawValue `asn1:"optional,tag:0"`
			Signers []asn1.RawValue `asn1:"set"`
		}
		rest, err = asn1.Unmarshal(sdDER, &sd)
		if err != nil || len(rest) != 0 || sd.Version != 1 || len(sd.Digests) != 0 || !sd.Content.Type.Equal(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}) || len(sd.Signers) != 0 || len(sd.Certs) > 16 {
			return nil, fmt.Errorf("invalid EST certs-only SignedData")
		}
		for _, raw := range sd.Certs {
			cert, err := x509.ParseCertificate(raw.FullBytes)
			if err != nil {
				return nil, err
			}
			certs = append(certs, cert)
		}
	}
	var roots []byte
	seen := map[string]bool{}
	for _, cert := range certs {
		if cert.IsCA && bytes.Equal(cert.RawIssuer, cert.RawSubject) && !seen[string(cert.Raw)] {
			seen[string(cert.Raw)] = true
			roots = append(roots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
		}
	}
	if err := validateTrustBundle(roots); err != nil {
		return nil, fmt.Errorf("discovered device roots: %w", err)
	}
	return roots, nil
}
