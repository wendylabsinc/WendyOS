package providers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"google.golang.org/grpc"
)

func TestStreamConsoleCancelsCloudConnection(t *testing.T) {
	started := make(chan struct{})
	setCloudRelayDialer(t, func(ctx context.Context, _ models.ExternalDevice) (*grpc.ClientConn, context.Context, error) {
		close(started)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		completed <- (&MicroWendyProvider{}).StreamConsole(ctx, models.ExternalDevice{
			ConnectionInfo: map[string]string{"type": "Cloud", "assetId": "asset"},
		}, func(liteclient.ConsoleChunk) error { return nil })
	}()
	<-started
	cancel()
	select {
	case err := <-completed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("connection error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("console connection ignored cancellation")
	}
}

func TestSerialRetryStopsOnCancellation(t *testing.T) {
	previous, previousDelay := connectSerialFn, serialConnectRetryDelay
	t.Cleanup(func() { connectSerialFn, serialConnectRetryDelay = previous, previousDelay })
	serialConnectRetryDelay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	connectSerialFn = func(gotCtx context.Context, _ *liteclient.WendyLiteClient, _ string) error {
		if gotCtx != ctx {
			t.Fatal("serial connection did not receive the caller context")
		}
		attempts++
		cancel()
		return errors.New("temporary serial failure")
	}
	if err := connectSerialWithRetry(ctx, nil, "test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("retry error = %v", err)
	}
	if attempts != 1 {
		t.Fatalf("made %d attempts after cancellation", attempts)
	}
}
