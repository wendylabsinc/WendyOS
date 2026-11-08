package services

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// pulseMetrics is trimmed pulse-export output: /scan published by two
// processes, 4242 counting it on both paths, plus a receive-only /odom.
const pulseMetrics = `# HELP ros2_pulse_topic_publish_rate_hertz Publish rate.
# TYPE ros2_pulse_topic_publish_rate_hertz gauge
ros2_pulse_topic_publish_rate_hertz{pid="4242",topic="/scan",path="inter"} 10.2
ros2_pulse_topic_publish_rate_hertz{pid="4242",topic="/scan",path="intra"} 9.8
ros2_pulse_topic_publish_rate_hertz{pid="4343",topic="/scan",path="inter"} 5
ros2_pulse_topic_publish_rate_hertz{pid="4343",topic="/scan_raw",path="inter"} 40
# TYPE ros2_pulse_topic_receive_rate_hertz gauge
ros2_pulse_topic_receive_rate_hertz{pid="4242",topic="/odom",path="inter"} 50
# TYPE ros2_pulse_topic_max_gap_seconds gauge
ros2_pulse_topic_max_gap_seconds{pid="4242",topic="/scan",side="pub"} 0.125
ros2_pulse_topic_max_gap_seconds{pid="4242",topic="/scan",side="recv"} 0.5
# TYPE ros2_pulse_process_window_seconds gauge
ros2_pulse_process_window_seconds{pid="4242"} 5
ros2_pulse_process_window_seconds{pid="4343"} 5
# TYPE ros2_pulse_process_last_window_timestamp_seconds gauge
ros2_pulse_process_last_window_timestamp_seconds{pid="4242"} 1762600000.5
ros2_pulse_process_last_window_timestamp_seconds{pid="4343"} 1762600001.25
# TYPE ros2_pulse_lines_skipped_total counter
ros2_pulse_lines_skipped_total 0
`

func TestParsePulseRate(t *testing.T) {
	rate, ok := parsePulseRate(strings.NewReader(pulseMetrics), "/scan")
	if !ok {
		t.Fatal("no rate for /scan")
	}
	want := pulseRate{hz: 15.2, windowS: 5, stamp: 1762600001.25, maxGapS: 0.125}
	if rate != want {
		t.Errorf("rate = %+v, want %+v", rate, want)
	}
	sample, ok := rate.sample()
	if !ok || sample.GetHz() != 15.2 || sample.GetWindow() != 76 || sample.GetMaxDelta() != 0.125 {
		t.Errorf("sample = %+v, %v", sample, ok)
	}
	for _, topic := range []string{"/odom", "/missing"} {
		if _, ok := parsePulseRate(strings.NewReader(pulseMetrics), topic); ok {
			t.Errorf("%s: got a publish rate, want none", topic)
		}
	}
}

func TestParsePromLine_EscapedLabels(t *testing.T) {
	name, labels, v, ok := parsePromLine(`m{pid="topic_freq.log",topic="/a\"b\\c",path="inter"} 2.5 1700000000000`)
	if !ok || name != "m" || v != 2.5 || labels["topic"] != `/a"b\c` || labels["path"] != "inter" {
		t.Errorf("got %q %v %v %v", name, labels, v, ok)
	}
	for _, line := range []string{"# TYPE m gauge", "", `m{topic="/x"} NaN`, `m{topic="/x} 1`, "m"} {
		if _, _, _, ok := parsePromLine(line); ok {
			t.Errorf("%q parsed, want skipped", line)
		}
	}
}

func TestSampleRejectsWhatTopicHzWouldNotPrint(t *testing.T) {
	for _, r := range []pulseRate{{hz: 0, windowS: 5}, {hz: 0.2, windowS: 5}} {
		if _, ok := r.sample(); ok {
			t.Errorf("%+v gave a sample", r)
		}
	}
}

func TestPulseTopicName(t *testing.T) {
	for in, want := range map[string]string{"/scan": "/scan", "scan": "/scan", "ns/scan": "/ns/scan"} {
		if got, ok := pulseTopicName(in); !ok || got != want {
			t.Errorf("pulseTopicName(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := pulseTopicName("~/scan"); ok {
		t.Error("private name mapped")
	}
}

// topicHzRuntime answers `ros2 topic hz` with one sample at 30.001 Hz.
func topicHzRuntime() *fakeROS2Runtime {
	return &fakeROS2Runtime{
		sidecar: ROS2Sidecar{Distro: "humble"},
		execFn: func(ctx context.Context, opts ROS2ExecOptions, stdout, stderr io.Writer) (int, error) {
			io.WriteString(stdout, "average rate: 30.001\n")
			io.WriteString(stdout, "\tmin: 0.032s max: 0.034s std dev: 0.00041s window: 31\n")
			return 0, nil
		},
	}
}

func pulseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestROS2Service_MonitorHz_ReadsPulseExport(t *testing.T) {
	rt := topicHzRuntime()
	svc := newTestROS2Service(t, rt, t.TempDir())
	svc.pulseExportURL = pulseServer(t, pulseMetrics).URL + "/metrics"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &fakeServerStream[agentpbv2.ROS2HzSample]{ctx: ctx, onSend: func(int) error {
		cancel()
		return nil
	}}
	err := withDeadline(t, func() error {
		return svc.MonitorHz(&agentpbv2.MonitorROS2HzRequest{Topic: "/scan"}, stream)
	})
	if err != nil {
		t.Fatalf("MonitorHz: %v", err)
	}
	if len(stream.sent) != 1 || stream.sent[0].GetHz() != 15.2 || stream.sent[0].GetWindow() != 76 {
		t.Fatalf("samples = %+v, want one at 15.2 Hz", stream.sent)
	}
	for _, c := range rt.calls {
		if len(c.Args) > 1 && c.Args[0] == "topic" && c.Args[1] == "hz" {
			t.Fatalf("ran ros2 %v although the exporter had the topic", c.Args)
		}
	}
}

func TestROS2Service_MonitorHz_FallsBackToTopicHz(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL + "/metrics"
	down.Close()

	for name, url := range map[string]string{
		"topic missing": pulseServer(t, pulseMetrics).URL + "/metrics",
		"exporter down": downURL,
		"not found":     pulseServer(t, "").URL + "/nope",
	} {
		t.Run(name, func(t *testing.T) {
			svc := newTestROS2Service(t, topicHzRuntime(), t.TempDir())
			svc.pulseExportURL = url
			stream := &fakeServerStream[agentpbv2.ROS2HzSample]{ctx: context.Background()}
			err := withDeadline(t, func() error {
				return svc.MonitorHz(&agentpbv2.MonitorROS2HzRequest{Topic: "/odom"}, stream)
			})
			if err != nil {
				t.Fatalf("MonitorHz: %v", err)
			}
			if len(stream.sent) != 1 || stream.sent[0].GetHz() != 30.001 {
				t.Fatalf("samples = %+v, want the topic hz one", stream.sent)
			}
		})
	}
}

func TestPulseExportURLFromEnv(t *testing.T) {
	t.Setenv(pulseExportEnv, " http://10.0.0.2:9000/metrics ")
	if got := pulseExportURLFromEnv(); got != "http://10.0.0.2:9000/metrics" {
		t.Errorf("got %q", got)
	}
	t.Setenv(pulseExportEnv, "")
	if got := pulseExportURLFromEnv(); got != "" {
		t.Errorf("empty override gave %q, want off", got)
	}
}
