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
	message := "TLS authentication failed. A wrong device clock or incompatible credentials may be responsible."
	if reason := certificateRefreshReason(err); reason != "" {
		message = "TLS authentication failed. " + reason
	}
	return message + "\n  " + strings.Join(tlsRecoveryNextSteps(err), "\n  ")
}

func tlsRecoveryNextSteps(err error) []string {
	if certificateRefreshReason(err) != "" {
		var rejection tlsHandshakeRejectedError
		errors.As(err, &rejection)
		return []string{refreshCertsStep(rejection.switchStep)}
	}
	return []string{
		"If the device clock may be wrong, run 'wendy device sync-time' on the same LAN, then retry.",
		"For connection details rerun with WENDY_TLS_DEBUG=1.",
	}
}

func stopConnectionRecovery(err error) bool {
	var org orgMismatchDeviceError
	var identity *certs.IdentityMismatchError
	var pin certs.BlockingPinError
	return errors.Is(err, context.Canceled) || errors.Is(err, ErrUserCancelled) ||
		errors.Is(err, errDeviceIdentityRefused) || errors.As(err, &org) ||
		errors.As(err, &identity) || errors.As(err, &pin)
}

// Both direct-device entry points use this sequence; a picker selection runs
// its clock half (connectPickedLANDevice). The caller supplies a retry of the
// same endpoint and pin key; recovery never selects another target.
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
	// Renewing the current context cannot fix a certificate another context
	// holds, or one whose context is unknown, and would spend its refresh
	// token (WDY-3864).
	var rejection tlsHandshakeRejectedError
	if errors.As(cause, &rejection) && rejection.switchStep != "" {
		fmt.Fprintln(os.Stderr, rejection.switchStep)
		return nil, cause
	}
	if !confirmFn("Refresh certificates and retry?") {
		return nil, cause
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := refreshCurrentCertsFn(ctx); err != nil {
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

// clockSkewSyncTimeout bounds each send of the proof. The first one includes
// the Roughtime query; later ones reuse the proof this process already holds.
const clockSkewSyncTimeout = 5 * time.Second

// clockSkewRetryDelays are the waits before each connection retry after the
// proof is sent. Nothing acknowledges the multicast proof, so the only
// confirmation the CLI can get is the device accepting the handshake it
// rejected. One datagram and one retry 1.5s later gave up on any proof that
// was lost or arrived before the agent had joined the group on that link. The
// agent rejoins the group on every interface every 5s
// (agent/timesync.multicastRefreshInterval), so the schedule spans two
// refreshes and the proof is sent again before every retry.
var clockSkewRetryDelays = []time.Duration{1500 * time.Millisecond, 3 * time.Second, 6 * time.Second}

var broadcastTimeFn = func(ctx context.Context) error {
	_, err := clitimesync.BroadcastTime(ctx)
	return err
}

// proofFromCacheFn reports whether the proof sent came from the on-disk cache
// (no time server answered) and its age. Indirected for tests.
var proofFromCacheFn = clitimesync.ProofFromCache

// Waiting for multicast delivery must not delay cancellation or start a new
// connection attempt after the caller's deadline has expired.
var waitForTimeProofFn = func(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func clockSkewRetryBudget() time.Duration {
	var total time.Duration
	for _, d := range clockSkewRetryDelays {
		total += d
	}
	return total
}

// One recovery attempt (a bounded series of sends and retries) per CLI
// invocation, even when multiple targets fail.
var clockSkewSyncAttempted bool

func isClockSkewSuspectError(err error) bool {
	// A pinned device whose candidate walk ended without a connection reports
	// the pinned refusal, which carries no TLS error text. If any address
	// rejected our certificate, that rejection is the same clock signal.
	var unauthenticated *noAuthenticatedEndpointError
	if errors.As(err, &unauthenticated) && unauthenticated.certSeen {
		return true
	}
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
	cached, age := proofFromCacheFn()
	source := ""
	if cached {
		source = fmt.Sprintf(" (no time server answered, so this is a proof saved %s ago)", age.Round(time.Minute))
	}
	fmt.Fprintf(os.Stderr, "Signed time proof broadcast%s. Retrying the connection for about %s to see whether the device accepted it...\n", source, clockSkewRetryBudget())
	sent := 1
	for i, delay := range clockSkewRetryDelays {
		if i > 0 {
			// The proof is one unacknowledged datagram per interface. Send it
			// again: the agent may have joined the group since the last one.
			sendCtx, cancel := context.WithTimeout(ctx, clockSkewSyncTimeout)
			if broadcastTimeFn(sendCtx) == nil {
				sent++
			}
			cancel()
		}
		if err := waitForTimeProofFn(ctx, delay); err != nil {
			return nil, err
		}
		conn, err := retry()
		if err == nil {
			fmt.Fprintln(os.Stderr, "Connected after broadcasting the time proof.")
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		if stopConnectionRecovery(err) || !isClockSkewSuspectError(err) {
			// The device now fails for another reason, which its error explains.
			fmt.Fprintln(os.Stderr, "Connection retry failed after broadcasting the time proof.")
			return nil, err
		}
		cause = err
	}
	next := "connect the device to the internet (Ethernet or Wi-Fi) so it can set its own clock, then retry."
	if cached {
		next = "connect this computer to the internet so it can fetch a fresh time proof, then retry."
	}
	times := fmt.Sprintf("%d times", sent)
	if sent == 1 {
		times = "once"
	}
	fmt.Fprintf(os.Stderr, "The device still rejected the connection after the time proof was sent %s, so its clock is probably still wrong.\n  Next: %s\n", times, next)
	return nil, cause
}
