package nancontrol

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestManagementTransactionWaitsAndCancellationHasNoEffect(t *testing.T) {
	unlock, err := Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("competing transaction did not wait: %v", err)
	}
	unlock()
	unlock, err = Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
