package backend

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
)

// SetMobileState is serialized separately from ordinary operations so it can
// cancel pending dials and drain a broker even while discovery is waiting on it.
// Account changes invalidate all old device handles, not just the mesh picker.
func (b *Backend) SetMobileState(active bool, account, organization string) error {
	b.mobileMu.Lock()
	defer b.mobileMu.Unlock()
	scope := account + "\x00" + organization
	changed := scope != b.mobileScope
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !active || changed {
		b.cancelOperation()
		b.ReserveConnection()
		// Retire device streams while their packet carrier can still deliver
		// TCP/TLS closure. Stopping the router first strands remote sessions.
		b.mu.Lock()
		b.disconnect()
		b.mu.Unlock()
		if err := sessionbroker.SetEmbeddedActive(ctx, false); err != nil {
			return err
		}
	}
	if active && account != "" {
		if err := commands.MobileUseAccount(account, organization); err != nil {
			return err
		}
	}
	b.mobileScope = scope
	return sessionbroker.SetEmbeddedActive(ctx, active && account != "")
}

func (b *Backend) MobileMeshSummary() (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return commands.MobileMeshSummary(ctx)
}
