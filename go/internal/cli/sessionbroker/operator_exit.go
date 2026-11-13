package sessionbroker

import (
	"context"
	"errors"
	"sync/atomic"
)

// Actors only nominate a cause. The owning Run writes it after joining every
// producer; no filesystem operation or contended lock enters a stop path.
type operatorExit struct {
	first atomic.Pointer[operatorExitCause]
}
type operatorExitCause struct {
	reason string
	err    error
}
type operatorExitKey struct{}

func validOperatorExitReason(reason string) bool {
	switch reason {
	case "context_done", "state_unavailable", "idle", "upstream_unavailable", "listener_failed", "server_ended", "startup_failed", "attachment_stopped", "credentials_changed", "disconnected", "intent_changed":
		return true
	}
	return false
}

func noteOperatorExit(ctx context.Context, reason string, err error) {
	if exit, ok := ctx.Value(operatorExitKey{}).(*operatorExit); ok && validOperatorExitReason(reason) {
		exit.first.CompareAndSwap(nil, &operatorExitCause{reason: reason, err: err})
	}
}

func operatorAttachmentExitReason(err error) string {
	switch {
	case errors.Is(err, errOperatorCredentialsChanged):
		return "credentials_changed"
	case errors.Is(err, errOperatorDisconnected):
		return "disconnected"
	default:
		return "attachment_stopped"
	}
}

// Keep the existing cancellation and discarded return-value behavior. Nominate
// the original terminal failure before cancellation can obscure its cause.
func runOperatorAttachment(ctx context.Context, cancel context.CancelFunc, run func() error) {
	defer cancel()
	err := run()
	if ctx.Err() == nil || !errors.Is(err, context.Canceled) {
		noteOperatorExit(ctx, operatorAttachmentExitReason(err), err)
	}
}
