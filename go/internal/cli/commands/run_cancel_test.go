package commands

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestRunWithInterruptChannelCancelsProviderBuildOnce(t *testing.T) {
	interrupts := make(chan os.Signal, 1)
	buildExited := false

	err := runWithInterruptChannel(context.Background(), interrupts, func(ctx context.Context) error {
		interrupts <- os.Interrupt
		<-ctx.Done()
		buildExited = true
		return errors.New("provider builder stopped")
	})

	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("err = %v, want ErrUserCancelled", err)
	}
	if !buildExited {
		t.Fatal("interrupt wrapper returned before provider build exited")
	}
}

// main's signal.NotifyContext cancels the parent on the same Ctrl-C. When it
// wins, the child context's cause is the parent's, so the wrapper must
// classify by the signal it received, not by the cause.
func TestRunWithInterruptChannelClassifiesInterruptWhenParentCancelsFirst(t *testing.T) {
	parent, cancelParent := context.WithCancelCause(context.Background())
	interrupts := make(chan os.Signal, 1)
	err := runWithInterruptChannel(parent, interrupts, func(ctx context.Context) error {
		cancelParent(errors.New("interrupt signal received"))
		<-ctx.Done()
		interrupts <- os.Interrupt // delivered only after the run noticed
		return errors.New("receiving container output: context canceled")
	})
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("err = %v, want ErrUserCancelled", err)
	}
}

// The attached single-container paths return ErrUserCancelled themselves
// after an interrupt, never nil, so this only covers runs that report success
// on their own.
func TestRunWithInterruptChannelInterruptAfterSuccessStaysSuccessful(t *testing.T) {
	interrupts := make(chan os.Signal, 1)
	err := runWithInterruptChannel(context.Background(), interrupts, func(ctx context.Context) error {
		interrupts <- os.Interrupt
		<-ctx.Done()
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil (a run that itself succeeded keeps its result)", err)
	}
}

func TestRunWithInterruptChannelSIGTERMIsNeverSuccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		note    func(context.Context)
		runErr  error
		wantMsg string
	}{
		{name: "during build", runErr: errors.New("docker buildx build failed: signal: terminated"), wantMsg: "wendy run was terminated"},
		{name: "attached app stopped", note: func(ctx context.Context) {
			noteInterruptedApp(ctx, "demo", interruptedAppStopped, nil)
		}, wantMsg: "wendy run was terminated; app demo was stopped"},
		{name: "app left running", note: func(ctx context.Context) {
			noteInterruptedApp(ctx, "demo", interruptedAppLeftRunning, nil)
		}, wantMsg: "wendy run was terminated; app demo is still running on the device"},
		{name: "stop failed", note: func(ctx context.Context) {
			noteInterruptedApp(ctx, "demo", interruptedAppStopFailed, errors.New("device unreachable"))
		}, wantMsg: "wendy run was terminated; stopping app demo failed: device unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signals := make(chan os.Signal, 1)
			err := runWithInterruptChannel(context.Background(), signals, func(ctx context.Context) error {
				signals <- syscall.SIGTERM
				<-ctx.Done()
				if tc.note != nil {
					tc.note(ctx)
				}
				return tc.runErr
			})
			if got := ErrorClass(err); got != "terminated" {
				t.Fatalf("class = %q (err %v), want terminated", got, err)
			}
			if err.Error() != tc.wantMsg {
				t.Fatalf("message = %q, want %q", err.Error(), tc.wantMsg)
			}
			// main's errorClass checks these before ErrorClass; wrapping either
			// would report the SIGTERM as an ordinary cancellation.
			if errors.Is(err, context.Canceled) || errors.Is(err, ErrUserCancelled) {
				t.Fatalf("terminated error %v reads as a cancellation", err)
			}
		})
	}
}

func TestRunWithInterruptChannelSIGTERMWinsOverInterrupt(t *testing.T) {
	signals := make(chan os.Signal, 2)
	err := runWithInterruptChannel(context.Background(), signals, func(ctx context.Context) error {
		signals <- os.Interrupt
		signals <- syscall.SIGTERM
		<-ctx.Done()
		return nil
	})
	if got := ErrorClass(err); got != "terminated" || !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("err = %v (class %q), want terminated", err, got)
	}
}
