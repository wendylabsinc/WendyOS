package services

import (
	"context"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"io"
	"os"
)

func worldRPCTiming(ctx context.Context) *rpctrace.Sink {
	return worldRPCTimingFor(ctx, os.Getenv("WENDY_WORLD_RPC_TIMING"), os.Stderr)
}

func worldRPCTimingFor(ctx context.Context, enabled string, writer io.Writer) *rpctrace.Sink {
	if enabled != "1" {
		return nil
	}
	sink, err := rpctrace.New(rpctrace.Config{})
	if err != nil {
		return nil
	}
	// Only this worker writes. RPCs update bounded in-memory counters and
	// enqueue without waiting. Shutdown seals without joining a blocked writer.
	go sink.Run(ctx, writer)
	return sink
}
