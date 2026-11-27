package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusiondemo"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func participantConfig() fusiondemo.Config {
	return fusiondemo.Config{Version: 1, ParticipantID: "atlas", RunID: "office", Frame: world.FrameRef{ID: "floor", Revision: "1"}, Sensors: []fusiondemo.Sensor{{SensorID: "camera-a", Kind: "camera", Owner: "hub"}, {SensorID: "camera-b", Kind: "camera", Owner: "hub"}, {SensorID: "gas-1", Kind: "gas", Owner: "hub"}, {SensorID: "audio-1", Kind: "audio", Owner: "hub"}, {SensorID: "lidar-atlas", Kind: "lidar", Owner: "atlas"}}}
}
func TestParticipantCommandHelper(t *testing.T) {
	if os.Getenv("WORLD_FUSION_PARTICIPANT_HELPER") != "1" {
		return
	}
	if err := serveParticipant(os.Stdin, os.Stdout, participantConfig(), os.Getenv("WORLD_FUSION_PARTICIPANT_DIR")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}
func participantProcess(t *testing.T, dir string, ticks ...fusiondemo.Tick) []fusiondemo.Output {
	t.Helper()
	var input bytes.Buffer
	enc := json.NewEncoder(&input)
	for _, tick := range ticks {
		if err := enc.Encode(tick); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestParticipantCommandHelper$")
	cmd.Env = append(os.Environ(), "WORLD_FUSION_PARTICIPANT_HELPER=1", "WORLD_FUSION_PARTICIPANT_DIR="+dir)
	cmd.Stdin = &input
	var output, stderr bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("participant process: %v %s", err, stderr.String())
	}
	dec := json.NewDecoder(&output)
	results := make([]fusiondemo.Output, len(ticks))
	for i := range results {
		if err := dec.Decode(&results[i]); err != nil {
			t.Fatal(err)
		}
	}
	return results
}
func TestParticipantProcessReopensOriginalNativeVersionAndExpiresDuplicates(t *testing.T) {
	cfg := participantConfig()
	s := sensing.Sample{SchemaVersion: sensing.Schema, ID: "office/lidar-atlas/1", SiteID: fusiondemo.Site, Frame: cfg.Frame, SensorID: "lidar-atlas", Kind: "lidar", Sequence: 1, SampleNS: int64(time.Second), RunID: cfg.RunID, ScenarioVersion: "v1", PackageSHA256: strings.Repeat("a", 64), SensorModel: "ray", PositionM: []float64{0, 0, 0}, UncertaintyM: .01, CalibrationRevision: "c1", Lidar: &sensing.Lidar{PointsM: [][3]float64{{0, 0, 1}}, MaxRangeM: 3}}
	raw, _ := json.Marshal(s)
	dir := t.TempDir()
	firstTick := fusiondemo.Tick{NowNS: int64(time.Second), LocalSamples: []json.RawMessage{raw}}
	first := participantProcess(t, dir, firstTick, firstTick)
	if first[0].Applied != 1 || first[1].Applied != 0 || first[1].Duplicates != 1 || len(first[1].Observations) != 1 {
		t.Fatal("first process did not retain native-version identity")
	}
	second := participantProcess(t, dir, fusiondemo.Tick{NowNS: int64(1500 * time.Millisecond)}, fusiondemo.Tick{NowNS: int64(4 * time.Second), Received: first[0].ReplicaRecords})
	if len(second[0].Observations) != 1 || second[0].ReplicaRecords[0].Version != first[0].ReplicaRecords[0].Version || second[0].Applied != 0 {
		t.Fatal("second process reissued or lost cached observation")
	}
	if second[1].Duplicates != 1 || len(second[1].Observations) != 0 || len(second[1].ReplicaRecords) != 1 {
		t.Fatal("reopened duplicate renewed expiry or discarded high-water")
	}
}
