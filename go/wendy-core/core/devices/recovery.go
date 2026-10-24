package devices

import (
	"context"
	"errors"
	"time"
)

// ExpiredClientCertificateReason captures local expiry evidence at dial failure.
// Unrelated org sessions cannot establish or disprove the selected org's expiry.
func ExpiredClientCertificateReason(org int32, credentials []Credential, now time.Time) string {
	count := 0
	for _, credential := range credentials {
		if org != 0 && credential.OrganizationID != int(org) {
			continue
		}
		count++
		if !credential.Expired(now) {
			return ""
		}
	}
	if count == 0 {
		return ""
	}
	return "The available client certificates are expired by this computer's clock. Check this computer's clock before refreshing."
}

// RecoveryEvidence contains host evidence captured for the failed target.
// Refused includes user cancellation and verified identity/org/SPKI mismatch.
// ClockSuspect includes the CLI's dial-ladder TLS-handshake rejection sentinel.
// RefreshReason includes locally observed expiry or a missing clientAuth EKU;
// a generic peer TLS expiry alert is insufficient. SwitchStep means the failing
// credential belongs to a different/unknown context, which must not be refreshed.
type RecoveryEvidence struct {
	Refused            bool
	ClockSuspect       bool
	ClockSyncAttempted bool
	RefreshReason      string
	SwitchStep         string
	Interactive        bool
}

// RecoveryDecision selects bounded recovery without changing endpoint or pins.
type RecoveryDecision struct{ SyncClock, OfferRefresh bool }

// DecideRecovery preserves the CLI's recovery order: terminal refusals stop;
// clock recovery precedes a credential prompt unless local expiry/clientAuth
// evidence already warrants refresh. Hosts retain prompting and all side effects.
func DecideRecovery(ctx context.Context, cause error, evidence RecoveryEvidence) RecoveryDecision {
	if StopRecovery(ctx, cause, evidence.Refused) {
		return RecoveryDecision{}
	}
	return RecoveryDecision{
		SyncClock:    evidence.RefreshReason == "" && evidence.ClockSuspect && !evidence.ClockSyncAttempted,
		OfferRefresh: evidence.Interactive && evidence.RefreshReason != "" && evidence.SwitchStep == "",
	}
}

// RetryHandshakeTimeout is the CLI's bounded timeout retry predicate. Explicit
// TLS rejections, cancellation and identity failures must never be retried here.
func RetryHandshakeTimeout(ctx context.Context, cause error, refused, explicitTLSRejection bool) bool {
	return !StopRecovery(ctx, cause, refused) && !errors.Is(cause, context.Canceled) && !explicitTLSRejection && IsReachabilityTimeoutError(cause)
}
