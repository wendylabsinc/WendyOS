package services

import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *DataService) Events(ctx context.Context, r *agentpbv2.DataEventsRequest) (*agentpbv2.DataEventsResponse, error) {
	if len(r.AppId) > 256 || len(r.Event) > 128 || len(r.Cursor) > 100 {
		return nil, status.Error(codes.InvalidArgument, "event filter exceeds limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	events, cursor, gap, err := s.manager.DeviceEvents(r.AppId, r.Event, r.Cursor, r.Replay)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	b, err := json.Marshal(events)
	if err != nil {
		return nil, err
	}
	return &agentpbv2.DataEventsResponse{EventsJson: b, Cursor: cursor, Gap: gap}, nil
}
