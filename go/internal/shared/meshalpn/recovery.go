package meshalpn

import (
	"crypto/tls"
	"log/slog"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

const issuerCooldown = 120 * time.Second

// Shared across material recreated by individual dials and across both TLS
// roles. No remote identity is needed before authentication. The deadline is
// monotonic in production and requires neither a timer nor retained peers.
var processRecovery = Recovery{now: time.Now}

// Recovery owns the cooldown for one runtime. The zero value is ready to use.
// Most callers use the process-wide default; embedded runtimes and tests can
// give independent simulated devices their own controller.
type Recovery struct {
	mu    sync.Mutex
	until time.Time
	now   func() time.Time
}

func (r *Recovery) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *Recovery) active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clock().Before(r.until)
}

func (r *Recovery) failed(err error, hinted bool) {
	if !hinted || !certs.IsIssuerPathError(err) {
		return
	}
	r.mu.Lock()
	now := r.clock()
	if now.Before(r.until) {
		r.mu.Unlock()
		return // Concurrent or fallback failures do not prolong the cooldown.
	}
	r.until = now.Add(issuerCooldown)
	r.mu.Unlock()
	slog.Warn("TLS issuer hints paused after issuer path failure", "duration", issuerCooldown, "error", err)
}

func (c *Certificates) recoveryState() *Recovery {
	if c.recovery != nil {
		return c.recovery
	}
	return &processRecovery
}

func fullExchange(config *tls.Config) {
	config.ClientSessionCache = nil
	config.SessionTicketsDisabled = true
	// A fresh exchange must not depend on a ticket's omitted issuers.
	config.WrapSession, config.UnwrapSession = nil, nil
}

// SetRecovery selects the runtime owning this material. Call only before the
// material is used by any handshake. A nil controller restores process scope.
func (c *Certificates) SetRecovery(recovery *Recovery) { c.recovery = recovery }
