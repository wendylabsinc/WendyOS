//go:build !windows

package commands

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
)

// interruptedByUser reads the cancel cause main's signal.NotifyContext sets.
// Deliver real signals to pin that the cause still names the signal.
func TestInterruptedByUserMatchesSignalNotifyContext(t *testing.T) {
	for _, tc := range []struct {
		sig  syscall.Signal
		want bool
	}{
		{syscall.SIGINT, true},
		{syscall.SIGTERM, false},
	} {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		if err := syscall.Kill(os.Getpid(), tc.sig); err != nil {
			stop()
			t.Fatal(err)
		}
		<-ctx.Done()
		if got := interruptedByUser(ctx); got != tc.want {
			t.Errorf("interruptedByUser after %v = %v, want %v (cause %v)", tc.sig, got, tc.want, context.Cause(ctx))
		}
		stop()
	}
	if interruptedByUser(context.Background()) {
		t.Error("a live context reads as interrupted")
	}
}
