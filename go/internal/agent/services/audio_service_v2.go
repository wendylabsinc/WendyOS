package services

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

// AudioServiceV2 implements agentpbv2.WendyAudioServiceServer by
// delegating to the v1 AudioService.
type AudioServiceV2 struct {
	agentpbv2.UnimplementedWendyAudioServiceServer
	v1 *AudioService
}

// NewAudioServiceV2 creates a new AudioServiceV2 wrapping the given v1 service.
func NewAudioServiceV2(v1 *AudioService) *AudioServiceV2 {
	return &AudioServiceV2{v1: v1}
}

func (s *AudioServiceV2) ListAudioDevices(ctx context.Context, req *agentpbv2.ListAudioDevicesRequest) (*agentpbv2.ListAudioDevicesResponse, error) {
	v1req := &agentpb.ListAudioDevicesRequest{}
	if req.TypeFilter != nil {
		t := agentpb.AudioDeviceType(*req.TypeFilter)
		v1req.TypeFilter = &t
	}
	v1resp, err := s.v1.ListAudioDevices(ctx, v1req)
	if err != nil {
		return nil, err
	}
	devices := make([]*agentpbv2.AudioDevice, len(v1resp.Devices))
	for i, d := range v1resp.Devices {
		devices[i] = &agentpbv2.AudioDevice{
			DeviceId:    d.Id,
			Name:        d.Name,
			Description: d.Description,
			Type:        agentpbv2.AudioDeviceType(d.Type),
			IsDefault:   d.IsDefault,
		}
	}
	return &agentpbv2.ListAudioDevicesResponse{Devices: devices}, nil
}

func (s *AudioServiceV2) SetDefaultAudioDevice(ctx context.Context, req *agentpbv2.SetDefaultAudioDeviceRequest) (*agentpbv2.SetDefaultAudioDeviceResponse, error) {
	v1resp, err := s.v1.SetDefaultAudioDevice(ctx, &agentpb.SetDefaultAudioDeviceRequest{DeviceId: req.DeviceId})
	if err != nil {
		return nil, err
	}
	return &agentpbv2.SetDefaultAudioDeviceResponse{
		Success:      v1resp.Success,
		ErrorMessage: v1resp.ErrorMessage,
	}, nil
}

func (s *AudioServiceV2) StreamAudioLevels(req *agentpbv2.StreamAudioLevelsRequest, stream grpc.ServerStreamingServer[agentpbv2.AudioLevelUpdate]) error {
	return s.v1.StreamAudioLevels(
		&agentpb.StreamAudioLevelsRequest{DeviceId: req.DeviceId, UpdateRateHz: req.UpdateRateHz},
		&audioLevelStreamAdapter{v2stream: stream},
	)
}

func (s *AudioServiceV2) StreamAudio(req *agentpbv2.StreamAudioRequest, stream grpc.ServerStreamingServer[agentpbv2.AudioChunk]) error {
	return s.v1.StreamAudio(
		&agentpb.StreamAudioRequest{DeviceId: req.DeviceId, SampleRate: req.SampleRate, Channels: req.Channels},
		&audioChunkStreamAdapter{v2stream: stream},
	)
}

type audioLevelStreamAdapter struct {
	v2stream grpc.ServerStreamingServer[agentpbv2.AudioLevelUpdate]
}

func (a *audioLevelStreamAdapter) Send(u *agentpb.AudioLevelUpdate) error {
	return a.v2stream.Send(&agentpbv2.AudioLevelUpdate{
		PeakDb:      u.PeakDb,
		RmsDb:       u.RmsDb,
		TimestampNs: u.TimestampNs,
	})
}
func (a *audioLevelStreamAdapter) SetHeader(md metadata.MD) error  { return a.v2stream.SetHeader(md) }
func (a *audioLevelStreamAdapter) SendHeader(md metadata.MD) error { return a.v2stream.SendHeader(md) }
func (a *audioLevelStreamAdapter) SetTrailer(md metadata.MD)       { a.v2stream.SetTrailer(md) }
func (a *audioLevelStreamAdapter) Context() context.Context        { return a.v2stream.Context() }
func (a *audioLevelStreamAdapter) SendMsg(m any) error             { return a.v2stream.SendMsg(m) }
func (a *audioLevelStreamAdapter) RecvMsg(m any) error             { return a.v2stream.RecvMsg(m) }

type audioChunkStreamAdapter struct {
	v2stream grpc.ServerStreamingServer[agentpbv2.AudioChunk]
}

func (a *audioChunkStreamAdapter) Send(c *agentpb.AudioChunk) error {
	return a.v2stream.Send(&agentpbv2.AudioChunk{
		PcmData:     c.PcmData,
		TimestampNs: c.TimestampNs,
		SampleRate:  c.SampleRate,
		Channels:    c.Channels,
	})
}
func (a *audioChunkStreamAdapter) SetHeader(md metadata.MD) error  { return a.v2stream.SetHeader(md) }
func (a *audioChunkStreamAdapter) SendHeader(md metadata.MD) error { return a.v2stream.SendHeader(md) }
func (a *audioChunkStreamAdapter) SetTrailer(md metadata.MD)       { a.v2stream.SetTrailer(md) }
func (a *audioChunkStreamAdapter) Context() context.Context        { return a.v2stream.Context() }
func (a *audioChunkStreamAdapter) SendMsg(m any) error             { return a.v2stream.SendMsg(m) }
func (a *audioChunkStreamAdapter) RecvMsg(m any) error             { return a.v2stream.RecvMsg(m) }
