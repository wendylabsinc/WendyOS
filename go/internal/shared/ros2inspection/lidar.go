package ros2inspection

import (
	_ "embed"
	"fmt"
	"math"
	"regexp"
)

// LiDAR summaries extend EchoTopic using an explicit, versioned handshake.
// Older agents ignore unknown metadata, so clients MUST verify the response
// acknowledgment before accepting any payload as a summary. JSON is valid YAML
// and travels in ROS2Message.Yaml; ordinary EchoTopic clients are unchanged.
const (
	LidarMetadata        = "x-wendy-ros2-lidar"
	LidarOptionsMetadata = "x-wendy-ros2-lidar-options"
	LidarVersion         = "1"
	LidarMaxSummaryBytes = 64 * 1024
)

//go:embed lidar_probe.py
var LidarProbeScript string

// LidarOptions selects a fixed read-only probe, never an arbitrary executable.
type LidarOptions struct {
	Topic           string  `json:"topic"`
	MessageType     string  `json:"message_type"`
	TargetFrame     string  `json:"target_frame"`
	DurationSeconds int     `json:"duration_seconds"`
	Count           int     `json:"count"`
	MaxPoints       int     `json:"max_points"`
	SamplePoints    int     `json:"sample_points"`
	MinZ            float64 `json:"min_z"`
	MaxZ            float64 `json:"max_z"`
	MinRange        float64 `json:"min_range"`
	MaxRange        float64 `json:"max_range"`
	UseSimTime      bool    `json:"use_sim_time"`
}

func DefaultLidarOptions() LidarOptions {
	return LidarOptions{MessageType: "sensor_msgs/msg/PointCloud2", DurationSeconds: 10, Count: 1,
		MaxPoints: 200000, SamplePoints: 32, MinZ: -1, MaxZ: 2, MinRange: 0.05, MaxRange: 20}
}

var lidarTopicPattern = regexp.MustCompile(`^/([A-Za-z_][A-Za-z0-9_]*)(/[A-Za-z_][A-Za-z0-9_]*)*$`)
var lidarFramePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_/-]*$`)

func (o LidarOptions) Validate() error {
	if len(o.Topic) > 255 || !lidarTopicPattern.MatchString(o.Topic) {
		return fmt.Errorf("topic must be an absolute ROS topic name of at most 255 bytes")
	}
	if o.MessageType != "sensor_msgs/msg/PointCloud2" && o.MessageType != "sensor_msgs/msg/LaserScan" {
		return fmt.Errorf("message_type must be sensor_msgs/msg/PointCloud2 or sensor_msgs/msg/LaserScan")
	}
	if o.TargetFrame != "" && (len(o.TargetFrame) > 255 || !lidarFramePattern.MatchString(o.TargetFrame)) {
		return fmt.Errorf("target_frame must be a relative TF frame identifier of at most 255 bytes")
	}
	for _, limit := range []struct {
		name        string
		n, min, max int
	}{
		{"duration_seconds", o.DurationSeconds, 1, 60}, {"count", o.Count, 1, 5},
		{"max_points", o.MaxPoints, 100, 1000000}, {"sample_points", o.SamplePoints, 0, 128},
	} {
		if limit.n < limit.min || limit.n > limit.max {
			return fmt.Errorf("%s must be an integer in %d..%d", limit.name, limit.min, limit.max)
		}
	}
	for _, limit := range []struct {
		name        string
		n, min, max float64
	}{
		{"min_z", o.MinZ, -100, 100}, {"max_z", o.MaxZ, -100, 100},
		{"min_range", o.MinRange, 0, 1000}, {"max_range", o.MaxRange, 0, 1000},
	} {
		if math.IsNaN(limit.n) || math.IsInf(limit.n, 0) || limit.n < limit.min || limit.n > limit.max {
			return fmt.Errorf("%s must be finite and in %g..%g", limit.name, limit.min, limit.max)
		}
	}
	if o.MinZ >= o.MaxZ {
		return fmt.Errorf("min_z must be less than max_z")
	}
	if o.MinRange >= o.MaxRange {
		return fmt.Errorf("min_range must be less than max_range")
	}
	return nil
}
