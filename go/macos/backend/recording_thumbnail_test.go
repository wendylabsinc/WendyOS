package backend

import (
	"context"
	"encoding/binary"
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestRecordingThumbnailFollowsCapturedSources(t *testing.T) {
	source := func(kind, id string, n uint64) data.SourceStats {
		return data.SourceStats{Source: data.Source{ID: id, Kind: kind}, Count: n}
	}
	m := data.Manifest{Sources: []data.SourceStats{source("ros2", "ros2:domain:/unused", 0), source("audio", "mic", 2), source("telemetry", "cpu", 16), source("camera", "front", 0)}}
	if kind, topics := recordingKind(&m); kind != "audio" || len(topics) != 0 {
		t.Fatalf("empty ROS or camera masked audio: %s, %v", kind, topics)
	}
	m.Sources[1].Count = 0
	if kind, _ := recordingKind(&m); kind != "dataset" {
		t.Fatalf("only telemetry captured: %s", kind)
	}
	if counts, _ := recordingPreview(&m, "dataset"); !slices.Equal(counts, []uint64{0, 0, 16, 0}) {
		t.Fatalf("wrong dataset cells: %v", counts)
	}
	m.Sources = append(m.Sources, source("ros2", "ros2:domain", 100), source("ros2", "ros2:domain:/imu", 7), source("ros2", "ros2:domain:/laser", 3))
	kind, topics := recordingKind(&m)
	counts, _ := recordingPreview(&m, kind)
	if kind != "rosbag" || !slices.Equal(topics, []string{"/imu", "/laser"}) || !slices.Equal(counts, []uint64{7, 3}) {
		t.Fatalf("expected only captured topics, got %s %v %v", kind, topics, counts)
	}
}

func TestRecordingThumbnailSelectsOneBoundedVerifiedFile(t *testing.T) {
	m := data.Manifest{Sources: []data.SourceStats{{Source: data.Source{ID: "primary", Kind: "audio"}, Count: 1}, {Source: data.Source{ID: "loopback", Kind: "audio"}, Count: 1}}}
	file := func(name, source string, n int64) data.File {
		return data.File{Path: name, SourceID: source, Size: n, SHA256: strings.Repeat("ab", 32)}
	}
	m.Files = []data.File{file("loopback/a.wav", "loopback", 777), file("primary/c.wav", "primary", 123), file("primary/b.wav", "primary", thumbnailFileLimit+1), file("../a.wav", "primary", 10), file("frame.jpg", "", 404), file("clip.mp4", "", thumbnailFileLimit+1)}
	_, audio := recordingPreview(&m, "audio")
	_, picture := recordingPreview(&m, "video")
	if audio == nil || audio.Path != "primary/c.wav" || picture == nil || picture.Path != "frame.jpg" {
		t.Fatalf("media selection: %+v %+v", audio, picture)
	}
	m.Files[4].SHA256 = "invalid"
	if _, f := recordingPreview(&m, "video"); f != nil {
		t.Fatalf("requested an unverified or oversized asset: %+v", f)
	}
}

func TestRecordingThumbnailWaveformPreservesSilenceAndPulsePosition(t *testing.T) {
	c := &visualFixtureClient{files: map[string][]byte{}}
	// Nine 50ms intervals; a single full-scale pulse in the fifth interval.
	raw := append(audioFixture()[:44], make([]byte, 9*4800)...)
	binary.LittleEndian.PutUint32(raw[4:], uint32(len(raw)-8))
	binary.LittleEndian.PutUint32(raw[40:], uint32(len(raw)-44))
	binary.LittleEndian.PutUint16(raw[44+4*4800:], 0x8000)
	addVisualFile(c, "audio/mic/first.wav", "mic", raw)
	file := c.manifest.Files[0]
	got, err := readRecordingThumbnail(context.Background(), c, "episode", file)
	if err != nil || !slices.Equal(got, []byte{0, 0, 0, 0, 255, 0, 0, 0, 0}) {
		t.Fatalf("peaks %v: %v", got, err)
	}
	c.mutate = func(chunk *pb.DataDownloadChunk) { chunk.Data = make([]byte, len(chunk.Data)) }
	if _, err = readRecordingThumbnail(context.Background(), c, "episode", file); err == nil {
		t.Fatal("rendered corrupted audio")
	}
}
