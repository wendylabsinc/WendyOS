package meshsharing

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Only authenticated, granted leases produce these events. No control payload,
// certificates or credentials are retained in diagnostics.
type controlEnd struct {
	role, phase, reason string
	peer                meshid.Device
	lease               tunnelLease
	expires             time.Time
	endedAt             time.Time
	elapsed             time.Duration
	heartbeats          uint64
	err, cleanupErr     error
}

type controlReporter interface {
	reportControlEnd(controlEnd)
}

func controlEndReason(ctx context.Context, expires time.Time, revoked <-chan struct{}, err error) string {
	if errors.Is(err, errControlCredentialInvalid) {
		return "identity-recheck-failed"
	}
	if ctx != nil && ctx.Err() != nil {
		return "local-shutdown"
	}
	if !expires.IsZero() && !time.Now().Before(expires) {
		return "credential-expired"
	}
	if revoked != nil {
		select {
		case <-revoked:
			return "policy-revoked"
		default:
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return "peer-closed"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "io-deadline"
	}
	return "io-error"
}
