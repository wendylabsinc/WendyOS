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
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Discovery reports observations, not diagnoses inferred from an mDNS TLS flag.
// Messages are controlled text: raw transport errors may contain credentials,
// terminal controls or misleading recovery advice from older connection paths.
func classifyLANProbeFailure(dev models.LANDevice, err error) *models.ProbeFailure {
	if err == nil {
		return nil
	}
	var org orgMismatchDeviceError
	var notListening agentNotListeningError
	switch {
	case isLANIdentityFailure(err):
		return lanIdentityFailure(dev, err)
	case expiredLoginFrom(err) != nil:
		login := expiredLoginFrom(err)
		return probeFailure("login_expired", login.message(), login.loginStep)
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

// lanProbeHint is the failure as one hint: its message, then its steps. A
// message laid out in lines keeps each step on a line of its own.
func lanProbeHint(lan *models.LANDevice) string {
	if lan == nil || lan.ProbeFailure == nil {
		return ""
	}
	separator := " "
	if strings.Contains(lan.ProbeFailure.Message, "\n") {
		separator = "\n"
	}
	return strings.Join(append([]string{lan.ProbeFailure.Message}, lan.ProbeFailure.NextSteps...), separator)
}

// Only observed local credential state warrants credential recovery advice.
func lanProbeFailure(dev models.LANDevice, err error, credentials []config.CertificateInfo) *models.ProbeFailure {
	failure := classifyLANProbeFailure(dev, err)
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
			return probeFailure("credentials_expired", "This CLI's stored client certificates are expired by this computer's clock.", "Check this computer's clock.", refreshCertsStep(switchContextStep(dev.OrgID)))
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

// lanIdentityFailure explains a device blocked because its identity or its
// certificate key changed, in the words the direct commands use for the same
// refusal (devicePinDiagnostic.changedIdentityWarning). It names the device as
// its discovery row does, and says which of the two changed when the error
// shows it.
func lanIdentityFailure(dev models.LANDevice, err error) *models.ProbeFailure {
	device := "The device"
	if name := tui.StripControl(discovery.SanitiseDisplayName(firstNonEmpty(dev.DisplayName, dev.Hostname))); name != "" {
		device = fmt.Sprintf("Device %q", name)
	}
	// Without a typed cause only the refusal itself is known.
	change, saved := "does not match the identity or certificate key this CLI saved for it", "identity"
	var explanation []string
	identityChanged := func(presentedAnother bool) {
		change, explanation = "identity changed", changedIdentityExplanation(false)
		if !presentedAnother {
			// A certificate that names no Wendy device is not another identity.
			explanation[0] = "It did not present the identity this CLI saved for it."
		}
	}
	var key, details string
	var refusal *deviceIdentityRefusalError
	var pin *devicepin.PinMismatchError
	var identity *certs.IdentityMismatchError
	switch {
	case errors.As(err, &pin):
		// The SPKI store's own key: the dialled name is often an IP that keys
		// nothing an unpin could clear. It is the identity the device presented,
		// and the unpin step is where it is shown.
		key, saved = pin.Key, "key"
		change, explanation = "certificate key changed", changedIdentityExplanation(true)
		details = fmt.Sprintf("Saved key: %s\nNow key:   %s", pin.Want, pin.Got)
	case errors.As(err, &refusal) && refusal.diagnostic != nil:
		key, details = refusal.diagnostic.hostname, refusal.diagnostic.details
		identityChanged(refusal.diagnostic.changed)
	case errors.As(err, &identity):
		was, now := identityMismatchSides(identity)
		details = fmt.Sprintf("Saved: %s\nNow:   %s", was, now)
		identityChanged(identity.GotPrincipal != "" || identity.GotAsset != "")
	}
	message := strings.Join(append([]string{device + " " + change + ", so the connection was blocked."}, explanation...), " ")
	// Render only known diagnostic fields, one to a line, with terminal
	// controls removed.
	for _, line := range strings.Split(details, "\n") {
		if line = tui.StripControl(line); line != "" {
			message += "\n" + line
		}
	}
	// One step, so the command never stands apart from its condition. Never
	// suggest a modified key: it could clear a different pin.
	step := "Keep the saved " + saved + " unless you expected this change."
	if key != "" && tui.StripControl(key) == key {
		step = "If you expected this change, run: wendy device unpin " + shellQuoteArg(key)
	}
	return probeFailure("device_identity_mismatch", message, step)
}
