package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
)

type inertServer struct {
	pb.UnimplementedWendyVideoServiceServer
	frame    *pb.VideoFrame
	requests chan *pb.StreamVideoRequest
}

func (s *inertServer) ListVideoDevices(context.Context, *pb.ListVideoDevicesRequest) (*pb.ListVideoDevicesResponse, error) {
	return &pb.ListVideoDevicesResponse{Devices: []*pb.VideoDevice{{Id: 42, StableId: "by-id:INERT",
		Thermal: &pb.ThermalCamera{Adapter: "topdon-tc001-hik-v1", CaptureWidth: 96, CaptureHeight: 200}}}}, nil
}

func (s *inertServer) StreamVideo(request *pb.StreamVideoRequest, stream grpc.ServerStreamingServer[pb.VideoFrame]) error {
	s.requests <- request
	for i := 0; i < 2; i++ {
		if err := stream.Send(s.frame); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func TestCapturePreservesActualRPCFrameAndReceiptWithoutClaimingHardware(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserves_frame", true: "rejects_wrong_geometry"}[bad], func(t *testing.T) {
			// A real private Unix gRPC transport, with explicitly inert payloads.
			root, err := os.MkdirTemp("", "topdon-rpc-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			path := filepath.Join(root, "agent.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			frame := &pb.VideoFrame{Data: bytes.Repeat([]byte{0xa5}, 38400), TimestampNs: uint64(time.Now().UnixNano()), Codec: pb.VideoCodec_VIDEO_CODEC_RAW,
				RawFormat: &pb.RawFormat{Width: 96, Height: 200, Fourcc: "YUYV", BytesPerLine: 192,
					ThermalPlane: &pb.ThermalPlane{Version: 1, Adapter: "topdon-tc001-hik-v1", Width: 96, Height: 96, RowStrideBytes: 192}}}
			if bad {
				frame.RawFormat.Height = 192
			}
			fixture := &inertServer{frame: frame, requests: make(chan *pb.StreamVideoRequest, 1)}
			server := grpc.NewServer()
			pb.RegisterWendyVideoServiceServer(server, fixture)
			go server.Serve(listener)
			defer server.Stop()
			conn, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out := filepath.Join(root, "recorded")
			err = capture(ctx, pb.NewWendyVideoServiceClient(conn), "by-id:INERT", out, 2)
			if bad && err == nil {
				t.Fatal("invalid geometry unexpectedly succeeded")
			}
			if !bad && err != nil {
				t.Fatal(err)
			}
			request := <-fixture.requests
			if request.GetStableId() != "by-id:INERT" || request.GetDeviceId() != 42 || request.GetCodec() != pb.VideoCodec_VIDEO_CODEC_RAW || request.GetWidth() != 0 || request.GetHeight() != 0 || request.GetFramerate() != 0 {
				t.Fatalf("unexpected request: %v", request)
			}
			manifest, err := os.ReadFile(filepath.Join(out, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var record recording
			if err := json.Unmarshal(manifest, &record); err != nil {
				t.Fatal(err)
			}
			if record.PhysicalAcceptance != "requires_identity_reference_and_replay_review" {
				t.Fatal("capture invented physical acceptance")
			}
			if bad {
				if record.Status != "incomplete" || len(record.Error) == 0 {
					t.Fatal("wrong geometry was accepted")
				}
				return
			}
			if record.Status != "captured" || len(record.Frames) != 2 {
				t.Fatalf("manifest: %+v", record)
			}
			for _, item := range record.Frames {
				encoded, err := os.ReadFile(filepath.Join(out, item.File))
				if err != nil {
					t.Fatal(err)
				}
				var restored pb.VideoFrame
				if err := protojson.Unmarshal(encoded, &restored); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(restored.Data, frame.Data) || restored.TimestampNs != frame.TimestampNs || item.ReceivedNS < int64(frame.TimestampNs) || item.SHA256 != digest(encoded) || item.RawSHA256 != digest(frame.Data) {
					t.Fatal("frame bytes/timestamps/hash were not preserved")
				}
			}
			if err := capture(ctx, pb.NewWendyVideoServiceClient(conn), "by-id:INERT", out, 2); err == nil {
				t.Fatal("overwrote an existing recording")
			}
		})
	}
}
