package commands

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
)

func TestDeployMetricsPropertiesForAChunkDeploy(t *testing.T) {
	m := &deployMetrics{
		started:        time.Now().Add(-5 * time.Second),
		command:        "wendy run",
		transport:      "chunk",
		targetPlatform: "linux/arm64",
		deviceType:     "jetson-orin-nano-devkit",
		chunk: &chunkDeployStats{
			imageBytes: 900_000_000,
			buildTime:  1500 * time.Millisecond,
			pushTime:   9 * time.Second,
			startTime:  700 * time.Millisecond,
			push: chunkPushSnapshot{
				SentChunks: 999, SentBytes: 62_900_000, TotalChunks: 5216,
				LayersTotal: 3, LayersReused: 2,
				UploadTime: 1800 * time.Millisecond, DeviceTime: 40500 * time.Millisecond,
			},
		},
	}
	p := m.properties(nil)
	want := map[string]string{
		"command_name":             "wendy run",
		"command_root":             "run",
		"success":                  "true",
		"deploy_transport":         "chunk",
		"deploy_target_platform":   "linux/arm64",
		"deploy_device_type":       "jetson-orin-nano-devkit",
		"deploy_build_ms":          "1500",
		"deploy_upload_ms":         "1800",
		"deploy_device_prepare_ms": "40500",
		"deploy_start_ms":          "700",
		"deploy_bytes_sent":        "62900000",
		"deploy_chunks_sent":       "999",
		"deploy_layers_reused":     "2",
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %q, want %q", k, p[k], v)
		}
	}
	if _, ok := p["deploy_fallback"]; ok {
		t.Error("a deploy that did not fall back must not report a fallback reason")
	}
}

func TestDeployMetricsEmitsOnlyOnceADeployPathWasChosen(t *testing.T) {
	var events []map[string]string
	analytics.SetTrackHookForTesting(func(event string, props map[string]string) {
		if event == "deploy_completed" {
			events = append(events, props)
		}
	})
	t.Cleanup(func() { analytics.SetTrackHookForTesting(nil) })

	(&deployMetrics{command: "wendy run"}).emit(errors.New("no Dockerfile"))
	if len(events) != 0 {
		t.Fatalf("emitted %d events before any deploy path ran", len(events))
	}
	(&deployMetrics{command: "wendy watch", transport: "registry", fallback: "unimplemented"}).emit(errors.New("boom"))
	if len(events) != 1 || events[0]["success"] != "false" || events[0]["deploy_fallback"] != "unimplemented" || events[0]["command_root"] != "watch" {
		t.Fatalf("events = %v", events)
	}
	for k, v := range events[0] {
		if strings.Contains(v, "boom") {
			t.Fatalf("property %s leaks error text: %q", k, v)
		}
	}
}

func TestChunkFallbackReason(t *testing.T) {
	cases := map[string]error{
		"unimplemented": status.Error(codes.Unimplemented, "old agent"),
		"transport":     status.Error(codes.Unavailable, "tunnel dropped"),
		"other":         status.Error(codes.Internal, "assembly failed"),
	}
	for want, err := range cases {
		if got := chunkFallbackReason(err); got != want {
			t.Errorf("chunkFallbackReason(%v) = %q, want %q", err, got, want)
		}
	}
	if got := chunkFallbackReason(io.ErrUnexpectedEOF); got != "transport" {
		t.Errorf("an EOF-shaped stream death is a transport failure, got %q", got)
	}
}
