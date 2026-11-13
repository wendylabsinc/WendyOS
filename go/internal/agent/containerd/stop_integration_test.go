//go:build linux

package containerd

import (
	"context"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/google/uuid"
)

// panickingSuppressor panics where StopContainer pauses the restart monitor,
// which is after the app has been marked as stopping.
type panickingSuppressor struct{}

func (panickingSuppressor) Suppress(string) func() { panic("injected stop panic") }

// WDY-3835: the gRPC interceptor recovers a panic in StopContainer, so the app
// must not stay marked as stopping. Otherwise every later stop and delete is
// refused with "app is currently being stopped" until the agent restarts.
// Runs against a live containerd; see unpack_integration_test.go.
func TestIntegrationStopContainerPanicClearsAppStopping(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	appID := "wdy3835-" + uuid.NewString()[:8]
	ctr, err := c.client.NewContainer(ctx, appID,
		containerd.WithNewSpec(),
		containerd.WithContainerLabels(map[string]string{labelKeyAppID: appID}),
	)
	if err != nil {
		t.Fatalf("creating container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Delete(context.WithoutCancel(ctx)) })

	c.SetRestartSuppressor(panickingSuppressor{})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("StopContainer returned without reaching the injected panic")
			}
		}()
		_ = c.StopContainer(ctx, appID)
	}()

	c.SetRestartSuppressor(nil)
	if err := c.StopContainer(ctx, appID); err != nil {
		t.Fatalf("StopContainer after a recovered panic: %v", err)
	}
}
