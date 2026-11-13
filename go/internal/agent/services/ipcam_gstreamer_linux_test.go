//go:build linux

package services

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Linux does not require GStreamer initialization on the process's initial OS
// thread, so the unit test can exercise the library runner directly. The Darwin
// path is covered by invoking the real helper binary in validation.
func TestGStreamerInProcessRunner(t *testing.T) {
	if _, err := loadGStreamer(); err != nil {
		t.Skip(err)
	}
	for _, tc := range []struct {
		name    string
		buffers int
		size    int
	}{
		{name: "single buffer", buffers: 1, size: 8},
		{name: "buffered output", buffers: 64, size: 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output []byte
			err := runIPCameraGStreamerPipeline(context.Background(), []string{
				"fakesrc", "num-buffers=" + strconv.Itoa(tc.buffers), "sizetype=fixed", "sizemax=" + strconv.Itoa(tc.size),
				"!", "fdsink", "fd=1",
			}, func(chunk []byte) error {
				output = append(output, chunk...)
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "stopped unexpectedly") {
				t.Fatalf("runner error = %v, want terminal pipeline error", err)
			}
			if want := tc.buffers * tc.size; len(output) != want {
				t.Fatalf("output bytes = %d, want %d", len(output), want)
			}
		})
	}
}

func TestGStreamerInProcessRunner_Cancellation(t *testing.T) {
	if _, err := loadGStreamer(); err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var outputBytes int
	err := runIPCameraGStreamerPipeline(ctx, []string{
		"fakesrc", "sizetype=fixed", "sizemax=8",
		"!", "fdsink", "fd=1",
	}, func(chunk []byte) error {
		outputBytes += len(chunk)
		cancel()
		return nil
	})
	if err != nil {
		t.Fatalf("runner error = %v, want nil on cancellation", err)
	}
	if outputBytes == 0 || ctx.Err() != context.Canceled {
		t.Fatalf("output bytes = %d, context error = %v, want output followed by cancellation", outputBytes, ctx.Err())
	}
}
