package liteenroll

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

const maxTrustBundle = 16384

// ProvisionTrust explicitly installs operator-supplied CA bundles in the device
// configuration. No certificates received from an endpoint become trust roots.
// Omitting all paths preserves compatibility with build-pinned firmware.
func ProvisionTrust(cfg *litepb.WendyConfEnrollment, devicePath, tsaPath, httpsPath string) (*http.Client, error) {
	return ProvisionTrustForProfile(cfg, devicePath, tsaPath, httpsPath, TrustProfile{})
}

// ProvisionTrustForProfile validates explicit bundles against the selected
// board's advertised verifier before changing its enrollment configuration.
func ProvisionTrustForProfile(cfg *litepb.WendyConfEnrollment, devicePath, tsaPath, httpsPath string, profile TrustProfile) (*http.Client, error) {
	if devicePath == "" && tsaPath == "" && httpsPath == "" {
		return http.DefaultClient, nil
	}
	if devicePath == "" || httpsPath == "" || (cfg.TimeUrl != "roughtime" && cfg.TimeUrl != DefaultTimeURL && tsaPath == "") {
		return nil, fmt.Errorf("USB trust provisioning requires --device-roots and --https-roots; a custom RFC 3161 endpoint also requires --tsa-roots")
	}
	bundles := make([][]byte, 3)
	for i, path := range []string{devicePath, tsaPath, httpsPath} {
		if i == 1 && path == "" && cfg.TimeUrl == DefaultTimeURL {
			if err := validateTrustBundleForProfile(defaultTSARoots, profile); err != nil {
				return nil, fmt.Errorf("bundled Sigstore TSA root: %w", err)
			}
			bundles[i] = bytes.Clone(defaultTSARoots)
			continue
		}
		if i == 1 && path == "" && cfg.TimeUrl == "roughtime" {
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
		if err := validateTrustBundleForProfile(bundles[i], profile); err != nil {
			return nil, fmt.Errorf("trust bundle %s: %w", path, err)
		}
	}
	client, err := trustHTTPClient(bundles[2])
	if err != nil {
		return nil, err
	}
	cfg.DeviceRoots, cfg.TsaRoots, cfg.HttpsRoots = bundles[0], bundles[1], bundles[2]
	cfg.ProvisionTrust = true
	return client, nil
}

func trustHTTPClient(httpsRoots []byte) (*http.Client, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(httpsRoots) {
		return nil, fmt.Errorf("HTTPS trust bundle contains no supported CA certificates")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return &http.Client{Transport: transport, Timeout: 15 * time.Second}, nil
}

func validateTrustBundle(data []byte) error {
	if len(data) == 0 || len(data) > maxTrustBundle {
		return fmt.Errorf("must contain 1–%d bytes of PEM CA certificates", maxTrustBundle)
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

// CheckTrustSupport runs before minting a credential so older firmware cannot
// silently ignore the new configuration fields.
func CheckTrustSupport(cfg *litepb.WendyConfEnrollment, challenge *litepb.WendyComEnrollmentChallenge) error {
	if !cfg.ProvisionTrust && (!challenge.GetBuiltinRootsReady() ||
		(cfg.TimeUrl != "roughtime" && !challenge.GetBuiltinTsaRootsReady())) {
		return fmt.Errorf("firmware has no confirmed built-in CA bundles; supply --device-roots and --https-roots (also --tsa-roots for RFC 3161), or install firmware with pinned roots; no cloud asset was reserved")
	}
	if cfg.ProvisionTrust && !challenge.GetUsbTrustSupported() {
		return fmt.Errorf("firmware does not support trust bundle provisioning; update the board first")
	}
	if cfg.ProvisionTrust {
		// Recheck the fresh challenge before minting, in case a reconnect or a
		// firmware change no longer advertises the initially selected verifier.
		profile := TrustProfileFromChallenge(challenge)
		for _, bundle := range []struct {
			name string
			data []byte
		}{{"device", cfg.DeviceRoots}, {"TSA", cfg.TsaRoots}, {"HTTPS", cfg.HttpsRoots}} {
			if bundle.name == "TSA" && cfg.TimeUrl == "roughtime" && len(bundle.data) == 0 {
				continue
			}
			if err := validateTrustBundleForProfile(bundle.data, profile); err != nil {
				return fmt.Errorf("%s roots: %w", bundle.name, err)
			}
		}
	}
	return nil
}
