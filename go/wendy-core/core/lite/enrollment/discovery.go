package liteenroll

import (
	"bytes"
	"context"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"mime"
	"net/url"
	"strings"

	litepb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

// DiscoverTrustForProfile selects authenticated CA roots compatible with the
// firmware. It publishes the new configuration only after all checks succeed.
func DiscoverTrustForProfile(ctx context.Context, client platform.LiteHTTPTransport, cfg *litepb.WendyConfEnrollment, endpoint string, profile TrustProfile) error {
	for _, bundle := range []struct {
		name string
		data []byte
	}{{"device", cfg.DeviceRoots}, {"HTTPS", cfg.HttpsRoots}} {
		if len(bundle.data) != 0 {
			if err := validateTrustBundleForProfile(bundle.data, profile); err != nil {
				return fmt.Errorf("existing %s roots: %w", bundle.name, err)
			}
		}
	}
	tsaRoots := cfg.TsaRoots
	if cfg.TimeUrl == DefaultTimeURL && len(tsaRoots) == 0 {
		tsaRoots = bytes.Clone(defaultTSARoots)
	}
	if cfg.TimeUrl != "roughtime" {
		if err := validateTrustBundleForProfile(tsaRoots, profile); err != nil {
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
		method := "HEAD"
		if fetchDeviceRoots {
			method = "GET"
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
		resp, err := client.DoLite(ctx, platform.HTTPRequest{Method: method, URL: u.String()})
		if err != nil {
			return fmt.Errorf("discovering PKI trust from %s: %w", u.Host, err)
		}

		if len(resp.VerifiedChains) == 0 {
			resp.Body.Close()
			return fmt.Errorf("CA discovery HTTPS connection was not verified")
		}
		for _, chain := range resp.VerifiedChains {
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
			if resp.StatusCode != 200 {
				return fmt.Errorf("CA discovery returned HTTP %d", resp.StatusCode)
			}
			if len(body) > 128*1024 {
				return fmt.Errorf("CA discovery response too large")
			}
			if profile.chainAlgorithm() != "" {
				deviceRoots, err = p256DiscoveryRoots(body, resp.Headers, profile.Inspector)
			} else {
				deviceRoots, err = discoveryRootsForProfile(body, resp.Headers.Get("Content-Type"), profile)
			}
			if err != nil {
				if profile.chainAlgorithm() != "" {
					return fmt.Errorf("PKI did not provide the requested P-256 device chain; no cloud asset was reserved: %w", err)
				}
				return err
			}
		} else {
			resp.Body.Close()
		}
	}
	if err := validateTrustBundleForProfile(httpsRoots, profile); err != nil {
		return fmt.Errorf("discovered HTTPS roots: %w", err)
	}
	cfg.DeviceRoots, cfg.TsaRoots, cfg.HttpsRoots = deviceRoots, tsaRoots, httpsRoots
	cfg.ProvisionTrust = true
	return nil
}

// EST responses contain untrusted intermediates as well as roots. Install only
// self-issued CAs from the authenticated discovery response.
func discoveryRoots(body []byte, contentType string, inspector platform.LiteCertificateInspector) ([]byte, error) {
	return discoveryRootsForProfile(body, contentType, TrustProfile{Inspector: inspector})
}

func discoveryRootsForProfile(body []byte, contentType string, profile TrustProfile) ([]byte, error) {
	certs, err := discoveryCertificates(body, contentType, profile.Inspector)
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

// discoveryCertificates parses a PEM or EST certs-only CMS cacerts body.
func discoveryCertificates(body []byte, contentType string, inspector platform.LiteCertificateInspector) ([]*platform.LiteCertificate, error) {
	var certs []*platform.LiteCertificate
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("-----BEGIN CERTIFICATE-----")) {
		if err := validateTrustBundle(body, inspector); err != nil {
			return nil, err
		}
		for rest := body; len(bytes.TrimSpace(rest)) > 0; {
			block, tail := pem.Decode(rest)
			cert, err := inspector.InspectCertificate(block.Bytes)
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
			cert, err := inspector.InspectCertificate(raw.FullBytes)
			if err != nil {
				return nil, err
			}
			certs = append(certs, cert)
		}
	}
	return certs, nil
}
