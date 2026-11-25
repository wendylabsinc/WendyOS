package commands

import (
	"context"
	"crypto/tls"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/failure"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// DesktopBLEAgentCertificates lists the operator certificates the app may
// present to an enrolled device's Bluetooth management door: the selected
// account first (the current context when account is unknown), then every
// other saved account holding a certificate, one per organization. The
// selected account is renewed best-effort first; an expired certificate is
// skipped. The device decides whether it accepts any of them.
func DesktopBLEAgentCertificates(ctx context.Context, account string) ([]config.CertificateInfo, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	ordered := make([]*config.AuthConfig, 0, len(cfg.Auth))
	if selected, ok := cfg.ContextByName(account); ok {
		ordered = append(ordered, selected)
	} else if current, err := config.ResolveAuth(cfg, "", nil); err == nil && current != nil {
		ordered = append(ordered, current)
	}
	for i := range cfg.Auth {
		if len(ordered) > 0 && cfg.Auth[i].Name == ordered[0].Name {
			continue
		}
		ordered = append(ordered, &cfg.Auth[i])
	}
	var out []config.CertificateInfo
	seen := map[string]bool{}
	now := time.Now()
	for i, auth := range ordered {
		if len(auth.Certificates) == 0 {
			continue
		}
		if i == 0 {
			// Offline renewal failure leaves the stored certificate in place.
			ensureFreshCertificateFn(ctx, auth) //nolint:errcheck
		}
		cert := auth.Certificates[0]
		if certExpired(cert, now) || seen[auth.OrganizationKey()] {
			continue
		}
		seen[auth.OrganizationKey()] = true
		out = append(out, cert)
	}
	if len(out) == 0 {
		return nil, failure.Wrap(errors.New("Sign in to Wendy Cloud with an account in this device's organization to change its Wi-Fi"), failure.AuthenticationRequired, failure.SignIn)
	}
	return out, nil
}

// DesktopBLEAgentTLSConfig is the app's side of the trust decision for an
// enrolled device's management door: the CLI's configuration, including the
// device's saved pin and, on first contact, the sign-in's tenant check.
func DesktopBLEAgentTLSConfig(device *models.BluetoothDevice, cert config.CertificateInfo) (*tls.Config, error) {
	return bleAgentTLSConfig(device, cert, openPinStore())
}

// DesktopBLEAgentOrganizationMismatch reports whether a Bluetooth connection
// failed only because the device belongs to another organization than the
// presented certificate, so another saved account may still be accepted.
func DesktopBLEAgentOrganizationMismatch(err error) bool {
	var org *certs.OrgMismatchError
	var tenant *BLETenantMismatchError
	return errors.As(err, &org) || errors.As(err, &tenant)
}
