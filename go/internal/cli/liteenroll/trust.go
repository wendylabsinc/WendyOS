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

// ProvisionTrust explicitly installs operator-supplied CA bundles in the USB
// configuration. No certificates received from an endpoint become trust roots.
// Omitting all paths preserves compatibility with build-pinned firmware.
func ProvisionTrust(cfg *litepb.WendyConfEnrollment, devicePath, tsaPath, httpsPath string) (*http.Client, error) {
	if devicePath == "" && tsaPath == "" && httpsPath == "" {
		return http.DefaultClient, nil
	}
	if devicePath == "" || httpsPath == "" || (cfg.TimeUrl != "roughtime" && tsaPath == "") {
		return nil, fmt.Errorf("USB trust provisioning requires --device-roots and --https-roots; RFC 3161 also requires --tsa-roots")
	}
	bundles := make([][]byte, 3)
	for i, path := range []string{devicePath, tsaPath, httpsPath} {
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
		if err := validateTrustBundle(bundles[i]); err != nil {
			return nil, fmt.Errorf("trust bundle %s: %w", path, err)
		}
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundles[2]) {
		return nil, fmt.Errorf("HTTPS trust bundle contains no supported CA certificates")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	cfg.DeviceRoots, cfg.TsaRoots, cfg.HttpsRoots = bundles[0], bundles[1], bundles[2]
	cfg.ProvisionTrust = true
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
		return fmt.Errorf("firmware does not support USB trust provisioning; update the board first")
	}
	return nil
}
