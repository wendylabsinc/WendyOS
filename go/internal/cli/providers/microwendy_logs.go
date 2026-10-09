package providers

import (
	"context"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// StreamConsole observes the firmware ring buffer without blocking board tasks.
// The lease expires if the CLI disappears; cancellation detaches immediately.
func (p *MicroWendyProvider) StreamConsole(ctx context.Context, device models.ExternalDevice, handle func(liteclient.ConsoleChunk) error) error {
	client, err := p.connectClient(device)
	if err != nil {
		return err
	}
	defer client.Close()
	chunks, detach, err := client.ConsoleAttach(true, false)
	if err != nil {
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
