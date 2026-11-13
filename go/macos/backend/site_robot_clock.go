package backend

import (
	"context"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A timestamp in a GetClock response was generated between send and receive.
// Use the upper offset bound, server-send, rather than assuming symmetric
// network latency. Subtracting it can only make a packet look older, never
// hide stale data. A short round trip bounds the added conservatism.
func robotClockSample(sent, received time.Time, serverNanos int64) (time.Duration, bool) {
	rtt := received.Sub(sent)
	if serverNanos <= 0 || rtt < 0 || rtt > 50*time.Millisecond {
		return 0, false
	}
	return time.Unix(0, serverNanos).Sub(sent), true
}

// Read only: this translates gateway timestamps to the client's clock without
// changing either device's system time. Keep a fixed offset for this connection;
// a later clock jump must fail freshness checks, not refresh old observations.
func measureRobotClock(ctx context.Context, client pb.WendyTimeSyncServiceClient) time.Duration {
	bestRTT := time.Hour
	var offset time.Duration
	for i := 0; i < 3; i++ {
		probe, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		sent := time.Now()
		response, err := client.GetClock(probe, &pb.GetClockRequest{})
		received := time.Now()
		cancel()
		if status.Code(err) == codes.Unimplemented {
			break
		}
		if err != nil {
			continue
		}
		sample, valid := robotClockSample(sent, received, response.GetUnixNanos())
		if valid && received.Sub(sent) < bestRTT {
			offset = sample
			bestRTT = received.Sub(sent)
		}
	}
	// Older gateways retain the existing strict wall-clock checks.
	return offset
}
