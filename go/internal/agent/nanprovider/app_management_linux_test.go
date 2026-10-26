//go:build linux

package nanprovider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/nancontrol"
)

func TestProviderManagementWaitsForAppPreparation(t *testing.T) {
	unlock, err := nancontrol.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, start := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		var err error
		if start {
			_, err = prepareSharedSession(ctx, testDevice(t, 1))
		} else {
			err = restartOwnedNAN(ctx, testDevice(t, 1))
		}
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("provider start=%v passed app reservation: %v", start, err)
		}
	}
}
