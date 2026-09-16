package services

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func validateRecordingExport(app, service, stream string) error {
	if err := appconfig.ValidateAppID(app); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if service != "" {
		if err := appconfig.ValidateServiceName(service); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	}
	if err := appconfig.ValidateRecordingStreams(map[string]appconfig.RecordingStream{stream: {Mode: "durable", MediaType: "application/octet-stream"}}); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

func (s *DataService) ExportRecording(req *agentpbv2.DataRecordingExportRequest, out grpc.ServerStreamingServer[recordingpb.StoredRecord]) error {
	if err := validateRecordingExport(req.AppId, req.Service, req.Stream); err != nil {
		return err
	}
	token, err := s.manager.ExportRecordingCheckpoint(req.AppId, req.Service, req.Stream, req.Checkpoint, func(r *recordingpb.StoredRecord) error {
		if err := out.Context().Err(); err != nil {
			return status.FromContextError(err).Err()
		}
		return out.Send(r)
	})
	if err == nil && token != "" {
		out.SetTrailer(metadata.Pairs("wendy-recording-checkpoint", token))
	}
	if status.Code(err) != codes.Unknown && err != nil {
		return err
	}
	return dataStatusError(err)
}

func (s *DataService) AcknowledgeRecordingExport(ctx context.Context, req *agentpbv2.DataRecordingExportAckRequest) (*agentpbv2.DataRecordingExportAckResponse, error) {
	if err := validateRecordingExport(req.AppId, req.Service, req.Stream); err != nil {
		return nil, err
	}
	if len(req.Checkpoint) > 128 {
		return nil, status.Error(codes.InvalidArgument, "invalid recording checkpoint")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	err := s.manager.AcknowledgeRecordingExport(req.AppId, req.Service, req.Stream, req.Checkpoint)
	if errors.Is(err, data.ErrInvalidRecordingCheckpoint) {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err != nil {
		return nil, dataStatusError(err)
	}
	return &agentpbv2.DataRecordingExportAckResponse{}, nil
}
