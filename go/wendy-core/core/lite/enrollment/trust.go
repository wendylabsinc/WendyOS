package liteenroll

import (
	"bytes"
	"encoding/pem"
	"fmt"
	"time"

	litepb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

const maxTrustBundle = 16384

func validateTrustBundle(data []byte, inspector platform.LiteCertificateInspector) error {
	if inspector == nil {
		return fmt.Errorf("host Lite certificate inspector is required")
	}
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
		cert, err := inspector.InspectCertificate(block.Bytes)
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
func CheckTrustSupport(cfg *litepb.WendyConfEnrollment, challenge *litepb.WendyComEnrollmentChallenge, inspector platform.LiteCertificateInspector) error {
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
		profile := TrustProfileFromChallenge(challenge, inspector)
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

// DefaultTSARoots returns the independently pinned Sigstore TSA root.
func DefaultTSARoots() []byte { return bytes.Clone(defaultTSARoots) }

// ProvisionTrustBundles validates host-read CA files before updating cfg. Nil
// bundles mean the corresponding path was omitted. Omitting all preserves
// firmware-pinned trust. A missing TSA bundle selects the pinned Sigstore root
// only at its default endpoint.
func ProvisionTrustBundles(cfg *litepb.WendyConfEnrollment, device, tsa, https []byte, profile TrustProfile) error {
	if device == nil && tsa == nil && https == nil {
		return nil
	}
	if device == nil || https == nil || (cfg.TimeUrl != "roughtime" && cfg.TimeUrl != DefaultTimeURL && tsa == nil) {
		return fmt.Errorf("USB trust provisioning requires --device-roots and --https-roots; a custom RFC 3161 endpoint also requires --tsa-roots")
	}
	if tsa == nil && cfg.TimeUrl == DefaultTimeURL {
		tsa = bytes.Clone(defaultTSARoots)
	}
	for _, bundle := range [][]byte{device, tsa, https} {
		if bundle == nil && cfg.TimeUrl == "roughtime" {
			continue
		}
		if err := validateTrustBundleForProfile(bundle, profile); err != nil {
			return err
		}
	}
	cfg.DeviceRoots, cfg.TsaRoots, cfg.HttpsRoots = bytes.Clone(device), bytes.Clone(tsa), bytes.Clone(https)
	cfg.ProvisionTrust = true
	return nil
}
