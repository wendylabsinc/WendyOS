//go:build !windows

package commands

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestLiteCameraSIGINTWithStalledClose(t *testing.T) {
	c := fakeLiteCamera()
	stubLiteCameraTarget(t, c)
	release := make(chan struct{})
	defer func() {
		close(release)
		select {
		case <-c.Done():
		case <-time.After(time.Second):
			t.Error("connection did not close after releasing stalled cleanup")
		}
	}()
	stalled := &stalledLiteCameraClient{fakeLiteCameraClient: c, operation: "close", release: release}
	connectLiteCameraFn = func(*SelectedDevice) (liteCameraClient, error) { return stalled, nil }

	// Use the same signal context as main. No frames arrive, matching a camera
	// that prints the streaming banner but never opens the player window.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	subscribed := make(chan struct{})
	c.onSubscribe = func() { close(subscribed) }
	cmd := newCameraViewCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--non-interactive"})
	result := make(chan error, 1)
	go func() { result <- cmd.ExecuteContext(ctx) }()
	select {
	case <-subscribed:
	case <-time.After(time.Second):
		t.Fatal("camera did not subscribe")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Ctrl+C returned an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ctrl+C did not exit camera view with stalled serial close")
	}
}
