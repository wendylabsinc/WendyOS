package liteenroll

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
)

const chainAlgorithmHeader = "X-Wendy-Chain-Algorithm"

// DiscoverP256DeviceRoots retrieves a tenant's Lite device authority from the
// selected PKI over verified HTTPS. Peer-supplied certificates are never used
// as trust anchors. Callers still verify the expected device identity and chain.
func DiscoverP256DeviceRoots(ctx context.Context, pkiEndpoint, tenantID string) ([]byte, error) {
	id, err := uuid.Parse(tenantID)
	if err != nil || id.String() != tenantID || id == uuid.Nil {
		return nil, fmt.Errorf("invalid tenant for Lite device CA discovery")
	}
	host := cloudenroll.PKISiblingHost(pkiEndpoint, "est")
	if host == "" {
		return nil, fmt.Errorf("selected session has no PKI CA discovery endpoint")
	}
	endpoint := "https://" + host + "/.well-known/est/" + tenantID + "/cacerts?chain_algorithm=" + cloudrequest.EnrollmentChainP256
	return discoverP256DeviceRoots(ctx, http.DefaultClient, endpoint)
}

func discoverP256DeviceRoots(ctx context.Context, client *http.Client, endpoint string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Fragment != "" {
		return nil, fmt.Errorf("Lite device CA discovery requires HTTPS without credentials or fragment")
	}
	secureClient := *client
	secureClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return fmt.Errorf("Lite device CA discovery redirects are not allowed")
	}
	resp, err := secureClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discovering P-256 device trust: %w", err)
	}
	defer resp.Body.Close()
	if resp.TLS == nil || len(resp.TLS.VerifiedChains) == 0 {
		return nil, fmt.Errorf("Lite device CA discovery HTTPS connection was not verified")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Lite device CA discovery returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 128*1024 {
		return nil, fmt.Errorf("Lite device CA discovery response too large")
	}
	return p256DiscoveryRoots(body, resp.Header)
}

func p256DiscoveryRoots(body []byte, header http.Header) ([]byte, error) {
	roots, err := discoveryRootsForProfile(body, header.Get("Content-Type"), TrustProfile{})
	if err != nil {
		return nil, fmt.Errorf("discovering P-256 device trust: %w", err)
	}
	// Older PKI servers may ignore unknown query parameters. Do not reserve a
	// credential for a new issuer after discovering a different CA hierarchy.
	values := header.Values(chainAlgorithmHeader)
	if len(values) != 1 || values[0] != cloudrequest.EnrollmentChainP256 {
		return nil, fmt.Errorf("PKI did not confirm the requested P-256 device chain; update PKI to support chain_algorithm=ECDSA_P256")
	}
	for rest := roots; len(rest) > 0; {
		block, tail := pem.Decode(rest)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		key, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok || key.Curve != elliptic.P256() || cert.SignatureAlgorithm != x509.ECDSAWithSHA256 || cert.CheckSignatureFrom(cert) != nil {
			return nil, fmt.Errorf("PKI did not return a self-signed P-256 device CA")
		}
		rest = tail
	}
	return roots, nil
}
