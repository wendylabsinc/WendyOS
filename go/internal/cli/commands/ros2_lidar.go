package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func newROS2LidarCmd() *cobra.Command {
	group := &cobra.Command{Use: "lidar", Short: "Sample ROS 2 LiDAR geometry"}
	opts := ros2inspection.DefaultLidarOptions()
	var domain int32
	var scope string
	sample := &cobra.Command{
		Use: "sample <topic>", Short: "Print bounded LiDAR summaries and XYZ samples as JSON lines",
		Long: `Decode PointCloud2 or LaserScan on the device. Return point counts,
range sectors and bounded XYZ samples. Source timestamps and frames are retained.
Use ros2 bag record for full-resolution SLAM data. Missing data or TF is unknown.
The default scope follows the running app; --scope host requires --domain.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Topic = args[0]
			if err := opts.Validate(); err != nil {
				return err
			}
			if scope != ros2inspection.AppScope && scope != ros2inspection.HostScope {
				return fmt.Errorf("scope must be app or host")
			}
			if scope == ros2inspection.HostScope && domain < 0 {
				return fmt.Errorf("host inspection requires --domain in 0..232")
			}
			if domain < -1 || domain > 232 {
				return fmt.Errorf("domain must be in 0..232")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Duration(opts.DurationSeconds)*time.Second)
			defer cancel()
			client, err := newROS2Client(ctx)
			if err != nil {
				return err
			}
			defer client.Close()
			encoded, _ := json.Marshal(opts)
			ctx = metadata.AppendToOutgoingContext(ctx,
				ros2inspection.ScopeMetadata, scope,
				ros2inspection.LidarMetadata, ros2inspection.LidarVersion,
				ros2inspection.LidarOptionsMetadata, string(encoded))
			stream, err := client.client.EchoTopic(ctx, &agentpbv2.EchoROS2TopicRequest{
				Topic: opts.Topic, Count: int32(opts.Count), DomainId: ros2DomainPtr(domain),
			}, grpc.MaxCallRecvMsgSize(ros2inspection.LidarMaxSummaryBytes+1024))
			if err != nil {
				return ros2RPCError(err)
			}
			return drainLidarSamples(stream, scope, opts, cmd.OutOrStdout())
		},
	}
	ros2DomainFlag(sample, &domain)
	f := sample.Flags()
	f.StringVar(&scope, "scope", ros2inspection.AppScope, "Inspection graph: app or host")
	f.StringVar(&opts.MessageType, "message-type", opts.MessageType, "sensor_msgs/msg/PointCloud2 or sensor_msgs/msg/LaserScan")
	f.StringVar(&opts.TargetFrame, "target-frame", "", "Transform samples into this TF frame at capture time")
	f.IntVar(&opts.DurationSeconds, "duration", opts.DurationSeconds, "Total observation deadline in seconds, 1..60")
	f.IntVar(&opts.Count, "count", opts.Count, "Number of summaries, 1..5")
	f.IntVar(&opts.MaxPoints, "max-points", opts.MaxPoints, "Maximum decoded points per message, 100..1000000")
	f.IntVar(&opts.SamplePoints, "sample-points", opts.SamplePoints, "Number of output XYZ samples, 0..128")
	f.Float64Var(&opts.MinZ, "min-z", opts.MinZ, "Minimum output-frame Z in metres")
	f.Float64Var(&opts.MaxZ, "max-z", opts.MaxZ, "Maximum output-frame Z in metres")
	f.Float64Var(&opts.MinRange, "min-range", opts.MinRange, "Minimum output-frame range in metres")
	f.Float64Var(&opts.MaxRange, "max-range", opts.MaxRange, "Maximum output-frame range in metres")
	f.BoolVar(&opts.UseSimTime, "use-sim-time", false, "Use ROS simulation time for TF")
	group.AddCommand(sample)
	return group
}

type lidarSampleStream interface {
	Header() (metadata.MD, error)
	Recv() (*agentpbv2.ROS2Message, error)
}

func drainLidarSamples(stream lidarSampleStream, scope string, opts ros2inspection.LidarOptions, out io.Writer) error {
	headers, err := stream.Header()
	if err != nil {
		return ros2RPCError(err)
	}
	versions := headers.Get(ros2inspection.LidarMetadata)
	if len(versions) != 1 || versions[0] != ros2inspection.LidarVersion {
		if _, err := stream.Recv(); err != nil && err != io.EOF {
			return ros2RPCError(err)
		}
		return fmt.Errorf("agent did not acknowledge LiDAR sampling; update the device agent")
	}
	if scope == ros2inspection.HostScope {
		scopes := headers.Get(ros2inspection.ScopeMetadata)
		if len(scopes) != 1 || scopes[0] != scope {
			return fmt.Errorf("agent did not acknowledge host inspection")
		}
	}
	for i := 0; i < opts.Count; i++ {
		msg, err := stream.Recv()
		if err == io.EOF {
			return fmt.Errorf("LiDAR stream ended after %d/%d summaries", i, opts.Count)
		}
		if err != nil {
			return ros2RPCError(err)
		}
		var envelope struct {
			SchemaVersion int    `json:"schema_version"`
			Topic         string `json:"topic"`
			MessageType   string `json:"message_type"`
			Status        string `json:"status"`
		}
		payload := msg.GetYaml()
		if len(payload) > ros2inspection.LidarMaxSummaryBytes || json.Unmarshal([]byte(payload), &envelope) != nil ||
			envelope.SchemaVersion != 1 || envelope.Topic != opts.Topic || envelope.MessageType != opts.MessageType ||
			(envelope.Status != "observed" && envelope.Status != "unknown") {
			return fmt.Errorf("agent returned an invalid LiDAR summary")
		}
		if _, err := fmt.Fprintln(out, payload); err != nil {
			return err
		}
		if envelope.Status == "unknown" {
			return fmt.Errorf("LiDAR observation is unknown; see the summary for details")
		}
	}
	return nil
}
