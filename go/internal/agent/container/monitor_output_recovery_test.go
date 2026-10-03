package container

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
)

type survivingOutputClient struct {
	*fakeContainerd
	recovered bool
}

func (f *survivingOutputClient) RecoverRunningTaskOutput(_ context.Context, publish func(string, services.ContainerOutput)) error {
	f.recovered = true
	publish("manual-app", services.ContainerOutput{Stdout: []byte("still-running\n")})
	publish("manual-app", services.ContainerOutput{Stderr: []byte("diagnostic\n")})
	return nil
}

func TestBootRecoveryCapturesOutputWithoutRestartEligibleApps(t *testing.T) {
	// --no-restart leaves no eligible boot entries; its surviving process still
	// needs a drain, with or without a configured log manager.
	for _, logging := range []bool{false, true} {
		f := &survivingOutputClient{fakeContainerd: &fakeContainerd{}}
		var lm *services.ContainerLogManager
		var output <-chan services.ContainerOutput
		if logging {
			lm = services.NewContainerLogManager(zap.NewNop(), services.NewTelemetryBroadcaster())
			_, output = lm.Subscribe("manual-app")
		}
		m := NewContainerMonitor(zap.NewNop(), f, lm, time.Second)
		m.ReconcileBootContainers(context.Background())
		if !f.recovered || len(f.startCallsSnapshot()) != 0 {
			t.Fatal("surviving output was not recovered independently of app restarts")
		}
		if logging {
			for _, expected := range []string{"still-running\n", "diagnostic\n"} {
				select {
				case got := <-output:
					if string(got.Stdout)+string(got.Stderr) != expected {
						t.Fatalf("lost recovered output: %+v", got)
					}
				case <-time.After(time.Second):
					t.Fatal("recovered output did not reach log subscribers")
				}
			}
		}
	}
}
