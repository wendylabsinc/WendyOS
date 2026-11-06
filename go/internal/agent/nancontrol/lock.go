// Package nancontrol coordinates radio-wide changes shared by the Wendy Net
// provider and applications with direct NAN control.
package nancontrol

import (
	"context"
)

var managementGate = make(chan struct{}, 1)

// Lock serializes the agent's check-and-reset and app preparation transactions.
// It must never cover a task's lifetime, shutdown wait, or output stream. The
// helper separately serializes individual commands from external host tools.
func Lock(ctx context.Context) (func(), error) {
	select {
	case managementGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-managementGate
			return nil, err
		}
		return func() { <-managementGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
