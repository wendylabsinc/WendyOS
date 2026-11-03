package backend

import (
	"context"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (b *Backend) NetworkTraffic(session uint64) (*commands.NetworkTraffic, error) {
	ctx, done := b.begin()
	defer done()
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stats, err := conn.ContainerService.GetNetworkStats(ctx, &pb.GetNetworkStatsRequest{})
	if err != nil {
		b.networkPrevious = nil
		if status.Code(err) == codes.Unimplemented {
			return nil, fmt.Errorf("agent does not support network traffic statistics: %w", err)
		}
		return nil, err
	}
	if b.networkSession != session || time.Since(b.networkSampleAt) > 10*time.Second {
		b.networkPrevious = nil
	}
	result := commands.NetworkTrafficBetween(b.networkPrevious, stats)
	b.networkPrevious, b.networkSession = stats, session
	b.networkSampleAt = time.Now()
	return result, nil
}
