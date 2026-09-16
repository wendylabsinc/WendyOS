package services

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *DataService) ExportRecording(req *agentpbv2.DataRecordingExportRequest, out grpc.ServerStreamingServer[recordingpb.StoredRecord]) error {
	if err := appconfig.ValidateAppID(req.AppId); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if req.Service != "" {
		if err := appconfig.ValidateServiceName(req.Service); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	}
	if err := appconfig.ValidateRecordingStreams(map[string]appconfig.RecordingStream{req.Stream: {Mode: "durable", MediaType: "application/octet-stream"}}); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	err := s.manager.ExportRecording(req.AppId, req.Service, req.Stream, func(r *recordingpb.StoredRecord) error {
		if err := out.Context().Err(); err != nil {
			return status.FromContextError(err).Err()
		}
		return out.Send(r)
	})
	if status.Code(err) != codes.Unknown && err != nil {
		return err
	}
	return dataStatusError(err)
}
