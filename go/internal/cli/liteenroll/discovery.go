package liteenroll

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/estcacerts"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

// DiscoverTrust bootstraps through the enrolling computer's HTTPS trust store.
// Only authenticated CA-discovery content and verified TLS trust anchors are
// provisioned; unverified peer certificates never become roots.
func DiscoverTrust(ctx context.Context, cfg *litepb.WendyConfEnrollment, endpoint string) (*http.Client, error) {
	return DiscoverTrustForProfile(ctx, cfg, endpoint, TrustProfile{})
}

// DiscoverTrustForProfile additionally checks the discovered CA algorithms
// against the selected board's advertised verifier.
func DiscoverTrustForProfile(ctx context.Context, cfg *litepb.WendyConfEnrollment, endpoint string, profile TrustProfile) (*http.Client, error) {
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("CA discovery redirects are not allowed")
	}}
	if err := discoverTrustForProfile(ctx, client, cfg, endpoint, profile); err != nil {
		return nil, err
	}
	return trustHTTPClient(cfg.HttpsRoots)
}

func discoverTrust(ctx context.Context, client *http.Client, cfg *litepb.WendyConfEnrollment, endpoint string) error {
	return discoverTrustForProfile(ctx, client, cfg, endpoint, TrustProfile{})
}

func discoverTrustForProfile(ctx context.Context, client *http.Client, cfg *litepb.WendyConfEnrollment, endpoint string, profile TrustProfile) error {
	for _, bundle := range []struct {
		name string
		data []byte
	}{{"device", cfg.DeviceRoots}, {"HTTPS", cfg.HttpsRoots}} {
		if len(bundle.data) != 0 {
			if err := validateTrustDomain(bundle.data, profile, bundle.name == "device"); err != nil {
				return fmt.Errorf("existing %s roots: %w", bundle.name, err)
			}
		}
	}
	tsaRoots := cfg.TsaRoots
	if cfg.TimeUrl == DefaultTimeURL && len(tsaRoots) == 0 {
		tsaRoots = bytes.Clone(defaultTSARoots)
	}
	if cfg.TimeUrl != "roughtime" {
		if err := validateTrustDomain(tsaRoots, profile, false); err != nil {
			return fmt.Errorf("RFC 3161 enrollment requires trusted TSA roots; use the default Sigstore endpoint or supply --device-roots, --https-roots and --tsa-roots: %w", err)
		}
	}
	discoverDevice := len(cfg.DeviceRoots) == 0
	if discoverDevice && endpoint == "" {
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
	deviceRoots := cfg.DeviceRoots
	httpsRoots := bytes.Clone(cfg.HttpsRoots)
	seen := map[string]bool{}
	for rest := httpsRoots; len(bytes.TrimSpace(rest)) > 0; {
		block, tail := pem.Decode(rest)
		if block == nil {
			return fmt.Errorf("invalid existing HTTPS roots")
		}
		seen[string(block.Bytes)] = true
		rest = tail
	}
	var targets []string
	if discoverDevice {
		targets = append(targets, endpoint, cfg.CsrUrl)
	}
	if cfg.TimeUrl != "roughtime" {
		targets = append(targets, cfg.TimeUrl)
	}
	for i, target := range targets {
		u, err := url.Parse(target)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("CA discovery requires an HTTPS URL without credentials or fragment")
		}
		fetchDeviceRoots := discoverDevice && i == 0
		method := http.MethodHead
		if fetchDeviceRoots {
			method = http.MethodGet
			algorithm := profile.chainAlgorithm()
			query, err := url.ParseQuery(u.RawQuery)
			if err != nil {
				return fmt.Errorf("invalid CA discovery query: %w", err)
			}
			if values, ok := query["chain_algorithm"]; ok && (len(values) != 1 || values[0] != algorithm) {
				return fmt.Errorf("CA discovery chain_algorithm conflicts with the firmware's enrollment profile")
			}
			if algorithm != "" {
				query.Set("chain_algorithm", algorithm)
				u.RawQuery = query.Encode()
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
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
		if fetchDeviceRoots {
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
			if profile.chainAlgorithm() != "" {
				deviceRoots, err = p256DiscoveryRoots(body, resp.Header)
			} else {
				deviceRoots, err = discoveryRootsForProfile(body, resp.Header.Get("Content-Type"), profile)
			}
			if err != nil {
				if profile.chainAlgorithm() != "" {
					return fmt.Errorf("PKI did not provide the requested P-256 device chain; no cloud asset was reserved: %w", err)
				}
				return err
			}
			if profile.meshTrust {
				discovered, err := discoveryCertificates(body, resp.Header.Get("Content-Type"))
				if err != nil {
					return err
				}
				meshRoots, intermediates, err :=
					discoverMeshTrust(ctx, client, u, cfg.TenantId, profile, discovered, deviceRoots)
				if err == nil && validateTrustBundleForProfile(meshRoots, profile) == nil {
					deviceRoots, cfg.LiteDeviceIntermediates = meshRoots, intermediates
				}
			}
		} else {
			resp.Body.Close()
		}
	}
	if err := validateTrustDomain(httpsRoots, profile, false); err != nil {
		return fmt.Errorf("discovered HTTPS roots: %w", err)
	}
	cfg.DeviceRoots, cfg.TsaRoots, cfg.HttpsRoots = deviceRoots, tsaRoots, httpsRoots
	cfg.ProvisionTrust = true
	return nil
}

// EST responses contain untrusted intermediates as well as roots. Install only
// self-issued CAs from the authenticated discovery response.
func discoveryRoots(body []byte, contentType string) ([]byte, error) {
	return discoveryRootsForProfile(body, contentType, TrustProfile{})
}

func discoveryRootsForProfile(body []byte, contentType string, profile TrustProfile) ([]byte, error) {
	certs, err := discoveryCertificates(body, contentType)
	if err != nil {
		return nil, err
	}
	var roots []byte
	seen := map[string]bool{}
	for _, cert := range certs {
		if cert.IsCA && bytes.Equal(cert.RawIssuer, cert.RawSubject) && !seen[string(cert.Raw)] {
			seen[string(cert.Raw)] = true
			roots = append(roots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
		}
	}
	if err := validateTrustBundleForProfile(roots, profile); err != nil {
		return nil, fmt.Errorf("discovered device roots: %w", err)
	}
	return roots, nil
}

func discoveryCertificates(body []byte, contentType string) ([]*x509.Certificate, error) {
	return estcacerts.ParseCertificates(body, contentType)
}
