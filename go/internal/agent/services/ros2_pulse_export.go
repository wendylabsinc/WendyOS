package services

import (
	"bufio"
	"context"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// ros2_pulse (https://github.com/TanayK07/ros2_pulse) is an LD_PRELOAD probe that
// counts per-topic publishes inside the app's own processes; its pulse-export
// tool serves those rates as Prometheus text. When it runs on the device,
// MonitorHz reads the rate from there instead of starting a fresh rclpy process
// per call for `ros2 topic hz`. Anything short of a usable rate falls back to
// `ros2 topic hz`.
const (
	// pulseExportEnv overrides the exporter URL. Set it to an empty string to
	// turn the exporter path off.
	pulseExportEnv        = "WENDY_PULSE_EXPORT_URL"
	defaultPulseExportURL = "http://127.0.0.1:10061/metrics"
	pulseExportTimeout    = 500 * time.Millisecond
	// pulseExportPoll matches pulse-export's own log poll; a sample is only sent
	// when the probe has written a new window (5 s by default).
	pulseExportPoll     = time.Second
	pulseExportMaxBytes = 8 << 20
)

var pulseExportClient = &http.Client{Timeout: pulseExportTimeout}

func pulseExportURLFromEnv() string {
	if v, ok := os.LookupEnv(pulseExportEnv); ok {
		return strings.TrimSpace(v)
	}
	return defaultPulseExportURL
}

// pulseRate is one topic's publish rate summed over the probed processes that
// publish it.
type pulseRate struct {
	hz      float64
	windowS float64 // longest window period among those processes
	stamp   float64 // newest window timestamp among them, Unix seconds
	maxGapS float64 // largest publish gap, 0 unless ROS_TOPIC_STATS_JITTER is on
}

// sample converts the rate to a ROS2HzSample, or returns false when it is not
// one `ros2 topic hz` could have printed (no messages, or fewer than two in the
// window). The exporter has no min or std dev, so those stay 0.
func (r pulseRate) sample() (*agentpbv2.ROS2HzSample, bool) {
	window := math.Round(r.hz * r.windowS)
	if r.hz <= 0 || window < 2 || window > math.MaxInt32 {
		return nil, false
	}
	return &agentpbv2.ROS2HzSample{Hz: r.hz, MaxDelta: r.maxGapS, Window: int32(window)}, true
}

// pulseTopicName returns the fully qualified name the probe reports for topic.
// A relative name resolves against the root namespace, as it does for
// `ros2 topic hz`; a private (~/) name has no node to resolve against.
func pulseTopicName(topic string) (string, bool) {
	if strings.HasPrefix(topic, "~") {
		return "", false
	}
	if !strings.HasPrefix(topic, "/") {
		topic = "/" + topic
	}
	return topic, true
}

// monitorHzFromPulse streams topic rates from pulse-export. It returns
// handled=false when the exporter is off, unreachable, or has no usable rate for
// the topic, including mid-stream, so the caller runs `ros2 topic hz` instead.
func (s *ROS2Service) monitorHzFromPulse(topic string, stream grpc.ServerStreamingServer[agentpbv2.ROS2HzSample]) (bool, error) {
	if s.pulseExportURL == "" {
		return false, nil
	}
	name, ok := pulseTopicName(topic)
	if !ok {
		return false, nil
	}
	ctx := stream.Context()
	ticker := time.NewTicker(pulseExportPoll)
	defer ticker.Stop()
	lastStamp := math.NaN()
	for {
		rate, ok := s.fetchPulseRate(ctx, name)
		if ctx.Err() != nil {
			return true, pulseStreamEnd(ctx)
		}
		if !ok {
			return false, nil
		}
		if rate.stamp != lastStamp {
			sample, ok := rate.sample()
			if !ok {
				return false, nil
			}
			lastStamp = rate.stamp
			if err := stream.Send(sample); err != nil {
				return true, err
			}
		}
		select {
		case <-ctx.Done():
			return true, pulseStreamEnd(ctx)
		case <-ticker.C:
		}
	}
}

// pulseStreamEnd mirrors how MonitorHz ends on a finished context: a deadline is
// an error, a client cancel is not.
func pulseStreamEnd(ctx context.Context) error {
	if ctx.Err() == context.DeadlineExceeded {
		return status.FromContextError(ctx.Err()).Err()
	}
	return nil
}

func (s *ROS2Service) fetchPulseRate(ctx context.Context, topic string) (pulseRate, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.pulseExportURL, nil)
	if err != nil {
		return pulseRate{}, false
	}
	resp, err := pulseExportClient.Do(req)
	if err != nil {
		return pulseRate{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return pulseRate{}, false
	}
	return parsePulseRate(io.LimitReader(resp.Body, pulseExportMaxBytes), topic)
}

// parsePulseRate reads the Prometheus text exposition from pulse-export and
// returns the publish rate of topic. One publish can count on both the inter and
// intra process path, so each process contributes its busier path; processes
// are summed, since `ros2 topic hz` sees every publisher.
func parsePulseRate(r io.Reader, topic string) (pulseRate, bool) {
	pubHz := map[string]float64{}
	gapS := map[string]float64{}
	windowS := map[string]float64{}
	stamp := map[string]float64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		name, labels, value, ok := parsePromLine(sc.Text())
		if !ok {
			continue
		}
		pid := labels["pid"]
		switch name {
		case "ros2_pulse_topic_publish_rate_hertz":
			if labels["topic"] == topic {
				pubHz[pid] = math.Max(pubHz[pid], value)
			}
		case "ros2_pulse_topic_max_gap_seconds":
			if labels["topic"] == topic && labels["side"] == "pub" {
				gapS[pid] = value
			}
		case "ros2_pulse_process_window_seconds":
			windowS[pid] = value
		case "ros2_pulse_process_last_window_timestamp_seconds":
			stamp[pid] = value
		}
	}
	if sc.Err() != nil || len(pubHz) == 0 {
		return pulseRate{}, false
	}
	var out pulseRate
	for pid, hz := range pubHz {
		out.hz += hz
		out.windowS = math.Max(out.windowS, windowS[pid])
		out.stamp = math.Max(out.stamp, stamp[pid])
		out.maxGapS = math.Max(out.maxGapS, gapS[pid])
	}
	return out, true
}

// parsePromLine parses one sample line of the text format:
// name{k="v",...} value [timestamp]. Comments, blank lines, malformed lines and
// non-finite values return ok=false.
func parsePromLine(line string) (name string, labels map[string]string, value float64, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", nil, 0, false
	}
	labels = map[string]string{}
	rest := ""
	if i := strings.IndexAny(line, "{ "); i < 0 {
		return "", nil, 0, false
	} else if line[i] == ' ' {
		name, rest = line[:i], line[i:]
	} else {
		name = line[:i]
		var good bool
		rest, good = parsePromLabels(line[i+1:], labels)
		if !good {
			return "", nil, 0, false
		}
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", nil, 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return "", nil, 0, false
	}
	return name, labels, v, true
}

// parsePromLabels fills labels from `k="v",...}` and returns what follows the
// closing brace. Values unescape \\, \" and \n.
func parsePromLabels(s string, labels map[string]string) (string, bool) {
	for {
		s = strings.TrimLeft(s, " ,")
		if strings.HasPrefix(s, "}") {
			return s[1:], true
		}
		eq := strings.Index(s, `="`)
		if eq <= 0 {
			return "", false
		}
		key := strings.TrimSpace(s[:eq])
		s = s[eq+2:]
		var val strings.Builder
		closed := false
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c == '"' {
				s, closed = s[i+1:], true
				break
			}
			if c == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(s[i])
				}
				continue
			}
			val.WriteByte(c)
		}
		if !closed {
			return "", false
		}
		labels[key] = val.String()
	}
}
