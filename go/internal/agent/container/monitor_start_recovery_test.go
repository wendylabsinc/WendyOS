package container

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"go.uber.org/zap"
)

type interruptedStartClient struct {
	*survivingOutputClient
	recoveredStart bool
	recoveryError  error
	listedAfter   bool
}

func (f *interruptedStartClient) RecoverInterruptedTaskStarts(context.Context) error {
	f.recoveredStart = true
	return f.recoveryError
}

func (f *interruptedStartClient) ListBootContainers(context.Context) ([]services.BootContainer, error) {
	f.listedAfter = f.recoveredStart && f.recovered
	return nil, nil // A --no-restart app is absent from the boot list.
}

func TestBootRecoversInterruptedStartsBeforeFilteringRestartIntent(t *testing.T) {
	for _, recoveryErr := range []error{nil, errors.New("one container could not be recovered")} {
		f := &interruptedStartClient{
			survivingOutputClient: &survivingOutputClient{fakeContainerd: &fakeContainerd{}},
			recoveryError: recoveryErr,
		}
		m := NewContainerMonitor(zap.NewNop(), f, nil, time.Second)
		m.ReconcileBootContainers(context.Background())
		if !f.listedAfter || len(f.startCallsSnapshot()) != 0 {
			t.Fatalf("held tasks and surviving output must be recovered before restart filtering: %+v", f)
		}
	}
}
