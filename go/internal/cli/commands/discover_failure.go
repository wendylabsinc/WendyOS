package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Discovery reports observations, not diagnoses inferred from an mDNS TLS flag.
// Messages are controlled text: raw transport errors may contain credentials,
// terminal controls or misleading recovery advice from older connection paths.
func classifyLANProbeFailure(err error) *models.ProbeFailure {
	if err == nil {
		return nil
	}
	var org orgMismatchDeviceError
	var notListening agentNotListeningError
	switch {
	case isLANIdentityFailure(err):
		return lanIdentityFailure(err)
	case errors.As(err, &org):
		return probeFailure("device_org_mismatch", "The device's certificate belongs to an organization for which this CLI has no credentials.", "Run 'wendy auth login' with an account that can access that organization.")
	case probeErrorMatches(err, isExplicitTLSRejection):
		return probeFailure("tls_rejected", "TLS authentication failed. A wrong device clock or incompatible credentials may be responsible.", "If the device clock may be wrong, run 'wendy device sync-time' on the same LAN, then retry.")
	case errors.As(err, &notListening), probeErrorMatches(err, isConnectionRefusedError):
		return probeFailure("connection_refused", "Agent unavailable (connection refused). The advertised service may be stopped or restarting.", "Retry shortly. If it persists, check the device's agent and enrollment state.")
	case errors.Is(err, context.DeadlineExceeded), status.Code(err) == codes.DeadlineExceeded, probeErrorMatches(err, isReachabilityTimeoutError):
		return probeFailure("timeout", "The agent did not respond before the probe deadline.", "Check the device's network connection and retry; a timeout does not establish an authentication problem.")
	case errors.Is(err, errTLSHandshakeRejected):
		return probeFailure("tls_rejected", "TLS authentication failed; discovery could not establish the cause.", "Retry with WENDY_TLS_DEBUG=1 to inspect the certificate and connection details.")
	case errors.Is(err, context.Canceled):
		return probeFailure("probe_cancelled", "Device verification was cancelled before it completed.", "Run 'wendy discover' again to retry verification.")
	case errors.Is(err, errProvisionedAgentUnauthorized):
		return missingLANCredentials()
	default:
		return probeFailure("probe_failed", "Discovery found the device, but could not verify its agent.", "Retry with WENDY_TLS_DEBUG=1 for connection details.")
	}
}

func probeFailure(code, message string, steps ...string) *models.ProbeFailure {
	return &models.ProbeFailure{Code: code, Message: message, NextSteps: steps}
}

func missingLANCredentials() *models.ProbeFailure {
	return probeFailure("credentials_missing", "This CLI has no client certificates to try the device's advertised secure endpoint.", "Run 'wendy auth login' with an account that can access the device's organization.")
}

func lanProbeHint(lan *models.LANDevice) string {
	if lan == nil || lan.ProbeFailure == nil {
		return ""
	}
	return strings.Join(append([]string{lan.ProbeFailure.Message}, lan.ProbeFailure.NextSteps...), " ")
}

// Only observed local credential state warrants credential recovery advice.
func lanProbeFailure(dev models.LANDevice, err error, credentials []config.CertificateInfo) *models.ProbeFailure {
	failure := classifyLANProbeFailure(err)
	if failure == nil || !dev.IsMTLS {
		return failure
	}
	var notListening agentNotListeningError
	if len(credentials) == 0 && !errors.As(err, &notListening) &&
		(failure.Code == "probe_failed" || failure.Code == "connection_refused" || failure.Code == "timeout") {
		return missingLANCredentials()
	}
	if failure.Code == "tls_rejected" && len(credentials) > 0 {
		allExpired := true
		for _, credential := range credentials {
			allExpired = allExpired && certExpired(credential, time.Now())
		}
		if allExpired {
			return probeFailure("credentials_expired", "This CLI's stored client certificates are expired by this computer's clock.", "Check this computer's clock, then run 'wendy auth refresh-certs' to renew expired credentials.")
		}
	}
	return failure
}

// Wrappers used by the dial ladder deliberately hide raw TLS text in Error().
// Inspect their causes, including each failed address, without rendering them.
func probeErrorMatches(err error, match func(error) bool) bool {
	if err == nil {
		return false
	}
	if match(err) {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range wrapped.Unwrap() {
			if probeErrorMatches(cause, match) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return probeErrorMatches(wrapped.Unwrap(), match)
	}
	return false
}

func isLANIdentityFailure(err error) bool {
	var identity *certs.IdentityMismatchError
	var pin certs.BlockingPinError
	return errors.Is(err, errDeviceIdentityRefused) || errors.As(err, &identity) || errors.As(err, &pin)
}

func lanIdentityFailure(err error) *models.ProbeFailure {
	message := "The device identity or certificate key differs from the saved pin; the connection was blocked."
	var key, details string
	var refusal *deviceIdentityRefusalError
	var pin *devicepin.PinMismatchError
	var identity *certs.IdentityMismatchError
	switch {
	case errors.As(err, &refusal) && refusal.diagnostic != nil:
		key = refusal.diagnostic.hostname
		details = refusal.diagnostic.details
	case errors.As(err, &pin):
		key = pin.Key
		details = fmt.Sprintf("Saved certificate key: %s. Presented certificate key: %s.", pin.Want, pin.Got)
	case errors.As(err, &identity):
		details = fmt.Sprintf("Saved: asset %s in organization %d. Now: asset %s in organization %d.", identity.WantAsset, identity.WantOrg, identity.GotAsset, identity.GotOrg)
	}
	// Render only known diagnostic fields, with terminal controls removed.
	if key != "" {
		message += fmt.Sprintf(" Saved pin: %s.", tui.StripControl(key))
	}
	if details != "" {
		message += " " + tui.StripControl(strings.ReplaceAll(details, "\n", " "))
	}
	step := "Verify which device is answering. Keep the saved pin unless you have confirmed an intentional identity change."
	// Never suggest a modified key: it could clear a different pin.
	if key != "" && tui.StripControl(key) == key {
		step += " After confirming, run: wendy device unpin " + shellQuoteArg(key)
	}
	return probeFailure("device_identity_mismatch", message, step)
}
