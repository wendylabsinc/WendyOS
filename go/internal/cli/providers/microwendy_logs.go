package providers

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// StreamConsole observes the firmware ring buffer without blocking board tasks.
// The lease expires if the CLI disappears. Cancellation closes the connection;
// delivery of the detach command is best-effort.
func (p *MicroWendyProvider) StreamConsole(ctx context.Context, device models.ExternalDevice, handle func(liteclient.ConsoleChunk) error) error {
	client, err := p.connectClient(ctx, device)
	if err != nil {
		return err
	}
	defer client.Close()
	// Attach and lease renewal wait for device replies without a deadline.
	// Closing concurrently wakes both, including a renewal that detach waits on.
	// Keep this callback active until detach has stopped local chunk delivery.
	stopCancel := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopCancel()
	chunks, detach, err := client.ConsoleAttach(true, false)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer detach(true)
	for {
		select {
		case <-ctx.Done():
			return nil
		case chunk, ok := <-chunks:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("Wendy Lite console connection closed")
			}
			if err := handle(chunk); err != nil {
				return err
			}
		}
	}
}
