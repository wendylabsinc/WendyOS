package literelay

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
	"strings"
	"time"

	"github.com/google/uuid"
)

// MaxCABundle is the largest PEM CA bundle a Lite board stores, and so the
// largest one accepted from CA discovery.
const MaxCABundle = 16384

// ChainAlgorithmP256 selects a tenant's classical Lite Device CA on the EST
// cacerts endpoint.
const ChainAlgorithmP256 = "ECDSA_P256"

const maxLiteDeviceCAChainResponse = 128 * 1024

// LiteDeviceCAChainURL is the EST endpoint serving tenantID's platform-chained
// Lite Device CA with its tenant and Customer Intermediates.
func LiteDeviceCAChainURL(estHost, tenantID string) (string, error) {
	id, err := uuid.Parse(tenantID)
	if err != nil || id.String() != tenantID || id == uuid.Nil {
		return "", fmt.Errorf("invalid tenant for Lite device CA discovery")
	}
	if estHost == "" || strings.ContainsAny(estHost, "/@?#") {
		return "", fmt.Errorf("invalid Lite device CA discovery host")
	}
	return "https://" + estHost + "/.well-known/est/" + tenantID + "/cacerts?chain_algorithm=" + ChainAlgorithmP256 + "&full_chain=true", nil
}

// FetchLiteDeviceCAChain returns the CA certificates of a full_chain cacerts
// answer as PEM. They are public, untrusted material: callers use them only as
// intermediates that must chain to their own trust, never as anchors, so the
// X-Wendy-Chain-Algorithm header is not required.
func FetchLiteDeviceCAChain(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Fragment != "" {
		return "", fmt.Errorf("Lite device CA discovery requires HTTPS without credentials or fragment")
	}
	secureClient := *client
	secureClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return fmt.Errorf("Lite device CA discovery redirects are not allowed")
	}
	resp, err := secureClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("discovering Lite device CA chain: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Lite device CA discovery returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLiteDeviceCAChainResponse+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxLiteDeviceCAChainResponse {
		return "", fmt.Errorf("Lite device CA discovery response too large")
	}
	certs, err := ParseCACerts(body, resp.Header.Get("Content-Type"))
	if err != nil {
		return "", fmt.Errorf("discovering Lite device CA chain: %w", err)
	}
	var chain []byte
	chained := false
	for _, cert := range certs {
		if !cert.IsCA {
			continue
		}
		chained = chained || !bytes.Equal(cert.RawIssuer, cert.RawSubject)
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	}
	// A PKI without full_chain support answers with the self-signed twin only.
	if !chained {
		return "", fmt.Errorf("PKI did not return the platform-chained Lite device CA; update PKI to support full_chain")
	}
	return string(chain), nil
}

// ParseCACerts parses a PEM or EST certs-only CMS cacerts body.
func ParseCACerts(body []byte, contentType string) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("-----BEGIN CERTIFICATE-----")) {
		if err := ValidateCABundle(body); err != nil {
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
		return certs, nil
	}
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil || media != "application/pkcs7-mime" {
		return nil, fmt.Errorf("CA discovery expected PEM or EST certs-only CMS")
	}
	der := body
	if len(der) == 0 || der[0] != 0x30 {
		der, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
		if err != nil {
			return nil, fmt.Errorf("invalid EST base64: %w", err)
		}
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
	return certs, nil
}

// ValidateCABundle accepts one to eight currently valid PEM CA certificates
// and nothing else, within MaxCABundle bytes.
func ValidateCABundle(data []byte) error {
	if len(data) == 0 || len(data) > MaxCABundle {
		return fmt.Errorf("must contain 1–%d bytes of PEM CA certificates", MaxCABundle)
	}
	count := 0
	for rest := bytes.TrimSpace(data); len(rest) > 0; {
		if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
			return fmt.Errorf("unexpected data outside a PEM certificate")
		}
		end := bytes.Index(rest, []byte("-----END CERTIFICATE-----"))
		if end < 0 {
			return fmt.Errorf("unterminated PEM certificate")
		}
		end += len("-----END CERTIFICATE-----")
		block, tail := pem.Decode(rest[:end])
		if block == nil || len(bytes.TrimSpace(tail)) != 0 || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return fmt.Errorf("invalid PEM certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("invalid certificate: %w", err)
		}
		if !cert.BasicConstraintsValid || !cert.IsCA {
			return fmt.Errorf("trust entries must be CA certificates")
		}
		if now := time.Now(); now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
			return fmt.Errorf("CA certificate is not currently valid")
		}
		count++
		if count > 8 {
			return fmt.Errorf("at most eight CA certificates are supported per bundle")
		}
		rest = bytes.TrimSpace(rest[end:])
	}
	if count == 0 {
		return fmt.Errorf("no CA certificates")
	}
	return nil
}
