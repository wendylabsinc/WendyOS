package backend

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"strings"
	"testing"
)

func TestChatDataSamplesBoundsAndNumericSummary(t *testing.T) {
	c := &visualFixtureClient{manifest: data.Manifest{ID: "e", CanonicalClock: "CLOCK_BOOTTIME", State: "complete"}, files: map[string][]byte{}}
	addVisualFile(c, "telemetry.jsonl", "telemetry", []byte("{\"episode_nanos\":-1000000000,\"values\":{\"load_1\":1}}\n{\"episode_nanos\":0,\"values\":{\"load_1\":3}}\n{\"episode_nanos\":1000000000,\"values\":{\"load_1\":99}}\n"))
	out := runDataTest(t, c, "device_data_samples", `{"episode":"e","kind":"telemetry","start_seconds":-1,"end_seconds":1,"limit":1}`)
	summary := out["numeric_summaries"].(map[string]any)["telemetry/load_1"].(map[string]any)
	if summary["mean"] != float64(2) || summary["count"] != float64(2) || out["truncated"] != true {
		t.Fatal(out)
	}
	c.mutate = func(chunk *pb.DataDownloadChunk) { chunk.Sha256 = "bad" }
	out = runDataTest(t, c, "device_data_samples", `{"episode":"e","kind":"telemetry"}`)
	if len(out["samples"].([]any)) != 0 || len(out["warnings"].([]any)) == 0 {
		t.Fatal("unverified bytes treated as samples", out)
	}
}
func TestChatDataSamplesMissingIsUnknown(t *testing.T) {
	c := &visualFixtureClient{manifest: data.Manifest{ID: "e", CanonicalClock: "CLOCK_BOOTTIME"}}
	out := runDataTest(t, c, "device_data_samples", `{"episode":"e","kind":"streams"}`)
	if len(out["warnings"].([]any)) < 1 || out["matching_decoded_samples"] != float64(0) {
		t.Fatal(out)
	}
}
func TestVisualROSLogDecoder(t *testing.T) {
	for _, big := range []bool{true, false} {
		w := newCDRFixture(big)
		w.u32(10)
		w.u32(20)
		w.raw = append(w.raw, 40)
		w.str("robot")
		w.str("motor fault")
		w.str("motor.cpp")
		w.str("step")
		w.u32(9)
		_, detail, err := decodeVisualROS("rcl_interfaces/msg/Log", w.raw)
		if err != nil || !strings.Contains(detail, "motor fault") || !strings.Contains(detail, "motor.cpp:9") {
			t.Fatal(detail, err)
		}
		if _, _, err = decodeVisualROS("rcl_interfaces/msg/Log", w.raw[:10]); err == nil {
			t.Fatal("accepted truncated log")
		}
	}
}
