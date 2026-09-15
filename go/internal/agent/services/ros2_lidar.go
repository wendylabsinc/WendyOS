package services

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func requestedLidarOptions(ctx context.Context, req *agentpbv2.EchoROS2TopicRequest) (*ros2inspection.LidarOptions, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	versions, values := md.Get(ros2inspection.LidarMetadata), md.Get(ros2inspection.LidarOptionsMetadata)
	if len(versions) == 0 && len(values) == 0 {
		return nil, nil
	}
	if len(versions) != 1 || versions[0] != ros2inspection.LidarVersion || len(values) != 1 || len(values[0]) > 4096 {
		return nil, status.Error(codes.InvalidArgument, "invalid LiDAR inspection version or options metadata")
	}
	var opts ros2inspection.LidarOptions
	decoder := json.NewDecoder(strings.NewReader(values[0]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&opts); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid LiDAR options: %v", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, status.Error(codes.InvalidArgument, "LiDAR options must be one JSON object")
	}
	if err := opts.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if opts.Topic != req.GetTopic() || opts.Count != int(req.GetCount()) {
		return nil, status.Error(codes.InvalidArgument, "LiDAR options must match the requested topic and count")
	}
	return &opts, nil
}

func (s *ROS2Service) echoLidar(req *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message], opts *ros2inspection.LidarOptions) error {
	// Queue the feature acknowledgment before the host resolver sends its scope
	// header. No payload can be accepted by a new client without both headers.
	if err := stream.SetHeader(metadata.Pairs(ros2inspection.LidarMetadata, ros2inspection.LidarVersion)); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(stream.Context(), time.Duration(opts.DurationSeconds)*time.Second)
	defer cancel()
	scs, err := s.resolveInspectionSidecars(ctx, req.DomainId)
	if err != nil {
		return err
	}
	sc := s.pickSidecarForTopic(ctx, scs, opts.Topic)
	scope, _ := requestedROS2Scope(ctx)
	if scope != ros2inspection.HostScope {
		if err := grpc.SendHeader(ctx, nil); err != nil {
			return err
		}
	}
	pr, pw := io.Pipe()
	execDone := make(chan error, 1)
	go func() {
		stderr := &ros2StderrTail{}
		code, execErr := s.runtime.ExecROS2(ctx, ROS2ExecOptions{DomainID: sc.domainID, SidecarName: sc.name, Lidar: opts}, pw, stderr)
		execErr = ros2StreamExitError(code, execErr, stderr)
		_ = pw.CloseWithError(execErr)
		execDone <- execErr
	}()
	// Closing the pipe is essential: a blocked writer cannot observe cancellation.
	defer func() { cancel(); _ = pr.CloseWithError(context.Canceled); <-execDone }()
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 4096), ros2inspection.LidarMaxSummaryBytes)
	sent := 0
	for scanner.Scan() {
		payload := scanner.Bytes()
		var envelope struct {
			SchemaVersion int    `json:"schema_version"`
			Topic         string `json:"topic"`
			Status        string `json:"status"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil || envelope.SchemaVersion != 1 || envelope.Topic != opts.Topic || (envelope.Status != "observed" && envelope.Status != "unknown") {
			return status.Error(codes.Internal, "LiDAR probe returned an invalid summary")
		}
		if err := stream.Send(&agentpbv2.ROS2Message{Topic: opts.Topic, Yaml: string(payload)}); err != nil {
			return err
		}
		sent++
		if sent >= opts.Count || envelope.Status == "unknown" {
			return nil
		}
	}
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	if err := scanner.Err(); err != nil {
		return status.Error(codes.Internal, fmt.Sprintf("LiDAR probe: %v", err))
	}
	if sent == 0 {
		return status.Error(codes.Unavailable, "LiDAR probe produced no summaries; check the topic type, DDS domain and publisher QoS")
	}
	return nil
}
