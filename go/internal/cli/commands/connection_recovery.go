package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	clitimesync "github.com/wendylabsinc/wendy/go/internal/cli/timesync"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// Capture local credential evidence when the dial fails, rather than reading
// mutable configuration while formatting an error. If the device's org is
// known, unrelated sessions cannot establish or disprove its credential expiry.
func expiredClientCertificateReason(org int32, credentials []config.CertificateInfo, now time.Time) string {
	count := 0
	for _, credential := range credentials {
		if org != 0 && credential.OrganizationID != int(org) {
			continue
		}
		count++
		if !certExpired(credential, now) {
			return ""
		}
	}
	if count == 0 {
		return ""
	}
	return "The available client certificates are expired by this computer's clock. Check this computer's clock before refreshing."
}

// A peer's generic TLS alert (including "expired certificate") describes its
// verdict using its own clock. Only local expiry evidence or a specific missing
// clientAuth capability warrants offering to reissue credentials.
func certificateRefreshReason(err error) string {
	if rejection, ok := err.(tlsHandshakeRejectedError); ok {
		if rejection.refreshReason != "" {
			return rejection.refreshReason
		}
		return certificateRefreshReason(rejection.cause)
	}
	if err == nil {
		return ""
	}
	if wrapped, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range wrapped.Unwrap() {
			if reason := certificateRefreshReason(cause); reason != "" {
				return reason
			}
		}
		return ""
	}
	if cause := errors.Unwrap(err); cause != nil {
		return certificateRefreshReason(cause)
	}
	if strings.Contains(err.Error(), "certificate is not valid for client authentication") {
		return "The client certificate does not permit client authentication."
	}
	return ""
}

func isCertRefreshableError(err error) bool { return certificateRefreshReason(err) != "" }

func tlsRecoveryMessage(err error) string {
	if reason := certificateRefreshReason(err); reason != "" {
		return "TLS authentication failed. " + reason + "\n  Run 'wendy auth refresh-certs', then retry."
	}
	return "TLS authentication failed. A wrong device clock or incompatible credentials may be responsible.\n  If the device clock may be wrong, run 'wendy device sync-time' on the same LAN, then retry.\n  For connection details rerun with WENDY_TLS_DEBUG=1."
}

func stopConnectionRecovery(err error) bool {
	var org orgMismatchDeviceError
	var identity *certs.IdentityMismatchError
	var pin certs.BlockingPinError
	return errors.Is(err, context.Canceled) || errors.Is(err, ErrUserCancelled) ||
		errors.Is(err, errDeviceIdentityRefused) || errors.As(err, &org) ||
		errors.As(err, &identity) || errors.As(err, &pin)
}

// Both direct-device entry points use this sequence. The caller supplies a
// retry of the same endpoint and pin key; recovery never selects another target.
func recoverAgentConnection(ctx context.Context, nonInteractive bool, cause error, retry func() (*grpcclient.AgentConnection, error)) (*grpcclient.AgentConnection, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if stopConnectionRecovery(cause) {
		return nil, cause
	}
	if !isCertRefreshableError(cause) {
		conn, err := autoSyncTimeAndRetry(ctx, cause, retry)
		if err == nil {
			return conn, nil
		}
		cause = err
	}
	return offerCertRefreshAndRetry(ctx, nonInteractive, cause, retry)
}

func offerCertRefreshAndRetry(ctx context.Context, nonInteractive bool, cause error, retry func() (*grpcclient.AgentConnection, error)) (*grpcclient.AgentConnection, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if stopConnectionRecovery(cause) || nonInteractive || jsonOutput || !isInteractiveTerminal() {
		return nil, cause
	}
	reason := certificateRefreshReason(cause)
	if reason == "" {
		return nil, cause
	}
	fmt.Fprintln(os.Stderr, reason)
	if !confirmFn("Refresh certificates and retry?") {
		return nil, cause
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := refreshAllCertsFn(ctx); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		fmt.Fprintf(os.Stderr, "Certificate refresh failed: %v\n", err)
		return nil, cause
	}
	conn, err := retry()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Certificates refreshed, but the connection retry failed.")
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "Connected after refreshing client certificates.")
	return conn, nil
}

const clockSkewSyncTimeout = 5 * time.Second
const clockSkewRetryDelay = 1500 * time.Millisecond

var broadcastTimeFn = func(ctx context.Context) error {
	_, err := clitimesync.BroadcastTime(ctx)
	return err
}

// Waiting for multicast delivery must not delay cancellation or start a new
// connection attempt after the caller's deadline has expired.
var waitForTimeProofFn = func(ctx context.Context) error {
	timer := time.NewTimer(clockSkewRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// One broadcast attempt per CLI invocation, even when multiple targets fail.
var clockSkewSyncAttempted bool

func isClockSkewSuspectError(err error) bool {
	// Keep the existing bounded recovery attempt for a dial-ladder rejection,
	// even when its final port attempt obscures the original TLS alert.
	return errors.Is(err, errTLSHandshakeRejected) || isTLSCertificateError(err)
}

func autoSyncTimeAndRetry(ctx context.Context, cause error, retry func() (*grpcclient.AgentConnection, error)) (*grpcclient.AgentConnection, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if stopConnectionRecovery(cause) || !isClockSkewSuspectError(cause) || clockSkewSyncAttempted {
		return nil, cause
	}
	clockSkewSyncAttempted = true
	fmt.Fprintln(os.Stderr, "Possible device clock error. Attempting to broadcast a signed time proof on the LAN...")
	syncCtx, cancel := context.WithTimeout(ctx, clockSkewSyncTimeout)
	syncErr := broadcastTimeFn(syncCtx)
	cancel()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if syncErr != nil {
		fmt.Fprintf(os.Stderr, "Could not broadcast a signed time proof: %v\n", syncErr)
		return nil, cause
	}
	fmt.Fprintln(os.Stderr, "Signed time proof broadcast; device receipt and clock change are unconfirmed. Retrying the connection...")
	if err := waitForTimeProofFn(ctx); err != nil {
		return nil, err
	}
	conn, err := retry()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Connection retry failed after broadcasting the time proof; device clock change remains unconfirmed.")
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "Connected after broadcasting the time proof.")
	return conn, nil
}
