package liteenroll

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

const chainAlgorithmHeader = "X-Wendy-Chain-Algorithm"

// DiscoverLiteDeviceCAChain fetches public Lite CAs as untrusted intermediates.
func DiscoverLiteDeviceCAChain(ctx context.Context, client platform.LiteHTTPTransport, inspector platform.LiteCertificateInspector, endpoint string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("Lite device CA discovery requires HTTPS without credentials or fragment")
	}
	resp, err := client.DoLite(ctx, platform.HTTPRequest{Method: "GET", URL: endpoint})

	if err != nil {
		return "", fmt.Errorf("discovering Lite device CA chain: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Lite device CA discovery returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024+1))
	if err != nil {
		return "", err
	}
	if len(body) > 128*1024 {
		return "", fmt.Errorf("Lite device CA discovery response too large")
	}
	certs, err := discoveryCertificates(body, resp.Headers.Get("Content-Type"), inspector)
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

func p256DiscoveryRoots(body []byte, header platform.HTTPHeaders, inspector platform.LiteCertificateInspector) ([]byte, error) {
	roots, err := discoveryRootsForProfile(body, header.Get("Content-Type"), TrustProfile{Inspector: inspector})
	if err != nil {
		return nil, fmt.Errorf("discovering P-256 device trust: %w", err)
	}
	// Older PKI servers may ignore unknown query parameters. Do not reserve a
	// credential for a new issuer after discovering a different CA hierarchy.
	var values []string
	for key, entries := range header {
		if strings.EqualFold(key, chainAlgorithmHeader) {
			values = append(values, entries...)
		}
	}
	if len(values) != 1 || values[0] != cloud.EnrollmentChainP256 {
		return nil, fmt.Errorf("PKI did not confirm the requested P-256 device chain; update PKI to support chain_algorithm=ECDSA_P256")
	}
	for rest := roots; len(rest) > 0; {
		block, tail := pem.Decode(rest)
		cert, err := inspector.InspectCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		if !cert.P256 || cert.SignatureAlgorithm != "ECDSA-SHA256" || !cert.SelfSignatureValid {
			return nil, fmt.Errorf("PKI did not return a self-signed P-256 device CA")
		}

		rest = tail
	}
	return roots, nil
}

// DeviceCAEndpoint derives the tenant-scoped public Lite intermediate discovery
// URL. The returned CA chain must never replace the operator's trust anchors.
func DeviceCAEndpoint(pkiEndpoint, tenantID string) (string, error) {
	if !canonicalUUID(tenantID) || tenantID == "00000000-0000-0000-0000-000000000000" {
		return "", fmt.Errorf("invalid tenant for Lite device CA discovery")
	}
	host := PKISiblingHost(pkiEndpoint, "est")
	if host == "" {
		return "", fmt.Errorf("selected session has no PKI CA discovery endpoint")
	}
	return "https://" + host + "/.well-known/est/" + tenantID + "/cacerts?chain_algorithm=" + cloud.EnrollmentChainP256 + "&full_chain=true", nil
}
