package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/episodeexport"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	recording "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func TestVisualSampleTimingPreservesPrerollAndGaps(t *testing.T) {
	manifest := data.Manifest{BootID: "boot-a", RequestBootNanos: 1000}
	times, err := visualTimes(&recording.SampleTiming{Clock: "CLOCK_BOOTTIME", BootId: "boot-a", Count: 3, TimestampsNanos: []int64{500, 1000, 3500}}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(times) != 3 || times[0] != -500 || times[1] != 0 || times[2] != 2500 {
		t.Fatalf("wrong canonical samples: %v", times)
	}
	start, period := int64(800), int64(250)
	times, err = visualTimes(&recording.SampleTiming{Clock: "CLOCK_BOOTTIME", BootId: "boot-a", Count: 3, StartNanos: &start, PeriodNanos: &period}, manifest)
	if err != nil || times[0] != -200 || times[2] != 300 {
		t.Fatalf("uniform samples: %v %v", times, err)
	}
}

func TestVisualTimingRefusesUnalignableClocksAndOverflow(t *testing.T) {
	manifest := data.Manifest{BootID: "boot-a", RequestBootNanos: 1000}
	start, period := int64(math.MaxInt64-1), int64(2)
	for _, timing := range []*recording.SampleTiming{
		nil,
		{Clock: "UNIX", BootId: "boot-a", Count: 1, TimestampsNanos: []int64{1000}},
		{Clock: "CLOCK_BOOTTIME", BootId: "boot-b", Count: 1, TimestampsNanos: []int64{1000}},
		{Clock: "CLOCK_BOOTTIME", BootId: "boot-a", Count: 2, TimestampsNanos: []int64{1000}},
		{Clock: "CLOCK_BOOTTIME", BootId: "boot-a", Count: 2, StartNanos: &start, PeriodNanos: &period},
	} {
		if times, err := visualTimes(timing, manifest); err == nil {
			t.Fatalf("aligned invalid timing: %+v => %v", timing, times)
		}
	}
}

func TestVisualTimeSeriesUsesAcquisitionNotReceiptTime(t *testing.T) {
	batch := &recording.TimeSeriesBatch{Timing: &recording.SampleTiming{Clock: "CLOCK_BOOTTIME", BootId: "boot-a", Count: 2, TimestampsNanos: []int64{900, 1200}}, Columns: []*recording.SampleColumn{{Values: &recording.SampleColumn_Float64Values{Float64Values: &recording.Float64Values{Values: []float64{21.2, 21.9}}}}}}
	payload, err := proto.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	r := &recording.StoredRecord{AppId: "sensors", Stream: "temperature", EpisodeNanos: 9000,
		Record: &recording.Record{Id: "batch-1", Payload: payload}, StreamDescriptor: &recording.StreamDescriptor{Schema: "wendy.agent.apps.v1.TimeSeriesBatch", Channels: []*recording.Channel{{Name: "temperature", Type: "float64", Unit: "Cel"}}}}
	var encoded bytes.Buffer
	if err := data.WriteRecording(&encoded, r); err != nil {
		t.Fatal(err)
	}
	v := &VisualSnapshot{}
	if err := v.readRecords(encoded.Bytes(), data.Manifest{BootID: "boot-a", RequestBootNanos: 1000}); err != nil {
		t.Fatal(err)
	}
	if len(v.Samples) != 2 || v.Samples[0].Time != -100 || v.Samples[1].Time != 200 || v.Samples[1].Value != 21.9 || v.Samples[1].Unit != "Cel" {
		t.Fatalf("wrong sample alignment: %+v", v.Samples)
	}
}

func TestVisualJSONMissingTimestampIsNotFabricated(t *testing.T) {
	v := &VisualSnapshot{}
	if err := v.readJSONLines([]byte("{\"name\":\"event\"}\n"), data.File{Path: "events.jsonl"}); err == nil {
		t.Fatal("accepted a sample without time")
	}
	if len(v.Samples) != 0 {
		t.Fatalf("fabricated samples: %+v", v.Samples)
	}
	if err := v.readJSONLines([]byte("{\"name\":\"trigger\",\"app_id\":\"app\",\"episode_nanos\":-200,\"client_timestamp_accepted\":false}\n"), data.File{Path: "events.jsonl"}); err != nil {
		t.Fatal(err)
	}
	if v.Samples[0].Time != -200 || !strings.Contains(v.Samples[0].Detail, "receipt") {
		t.Fatalf("lost provenance: %+v", v.Samples[0])
	}
}

type visualFixtureClient struct {
	pb.DataServiceClient
	manifest data.Manifest
	files    map[string][]byte
	mutate   func(*pb.DataDownloadChunk)
}

func (c *visualFixtureClient) Inspect(context.Context, *pb.DataInspectRequest, ...grpc.CallOption) (*pb.DataInspectResponse, error) {
	raw, err := json.Marshal(c.manifest)
	return &pb.DataInspectResponse{ManifestJson: raw}, err
}
func (c *visualFixtureClient) Download(_ context.Context, req *pb.DataDownloadRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[pb.DataDownloadChunk], error) {
	payload := c.files[req.Path]
	sum := sha256.Sum256(payload)
	var chunks []*pb.DataDownloadChunk
	for offset := 0; offset < len(payload); {
		end := min(offset+(1<<20), len(payload))
		chunk := &pb.DataDownloadChunk{Path: req.Path, Offset: int64(offset), Data: payload[offset:end], Size: int64(len(payload)), Sha256: hex.EncodeToString(sum[:]), Eof: end == len(payload)}
		if c.mutate != nil {
			c.mutate(chunk)
		}
		chunks = append(chunks, chunk)
		offset = end
	}
	return &visualFixtureStream{chunks: chunks}, nil
}

type visualFixtureStream struct {
	grpc.ClientStream
	chunks []*pb.DataDownloadChunk
}

func (s *visualFixtureStream) Recv() (*pb.DataDownloadChunk, error) {
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	chunk := s.chunks[0]
	s.chunks = s.chunks[1:]
	return chunk, nil
}

func TestVisualDownloadsValidateOffsetsMetadataAndChecksum(t *testing.T) {
	payload := []byte("{\"episode_nanos\":0,\"name\":\"trigger\"}\n")
	sum := sha256.Sum256(payload)
	meta := data.File{Path: "events.jsonl", Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}
	for name, mutate := range map[string]func(*pb.DataDownloadChunk){
		"offset":   func(c *pb.DataDownloadChunk) { c.Offset = 1 },
		"size":     func(c *pb.DataDownloadChunk) { c.Size++ },
		"path":     func(c *pb.DataDownloadChunk) { c.Path = "other" },
		"checksum": func(c *pb.DataDownloadChunk) { c.Data = bytes.Repeat([]byte{'x'}, len(c.Data)) },
	} {
		t.Run(name, func(t *testing.T) {
			c := &visualFixtureClient{files: map[string][]byte{meta.Path: payload}, mutate: mutate}
			if _, err := visualDownload(context.Background(), c, "episode", meta); err == nil {
				t.Fatal("accepted corrupted download")
			}
		})
	}
	c := &visualFixtureClient{files: map[string][]byte{meta.Path: payload}}
	if raw, err := visualDownload(context.Background(), c, "episode", meta); err != nil || !bytes.Equal(raw, payload) {
		t.Fatalf("valid download failed: %q %v", raw, err)
	}
}

func TestVisualEpisodeAlignsCameraAndEventAndRetainsLimits(t *testing.T) {
	event := []byte("{\"episode_nanos\":100,\"name\":\"human\",\"app_id\":\"detector\"}\n")
	camera := []byte("{\"canonical_episode_nanos\":-500,\"sample_id\":20}\n{\"canonical_episode_nanos\":100,\"sample_id\":21}\n")
	manifest := data.Manifest{ID: "episode", State: "complete", CanonicalClock: "CLOCK_BOOTTIME", BootID: "boot-a", StoppedEpisodeNS: 1000}
	files := map[string][]byte{"events.jsonl": event, "cameras/front/index.jsonl": camera}
	for name, raw := range files {
		sum := sha256.Sum256(raw)
		manifest.Files = append(manifest.Files, data.File{Path: name, Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:]), SourceID: "front"})
	}
	manifest.Files = append(manifest.Files, data.File{Path: data.RecordingLogFile, Size: visualFileLimit + 1})
	c := &visualFixtureClient{manifest: manifest, files: files}
	v, err := readVisualEpisode(context.Background(), c, "episode")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Samples) != 3 || v.Start != -500 || v.End != 1000 || v.Samples[1].Time != v.Samples[2].Time {
		t.Fatalf("timeline is not aligned: %+v", v)
	}
	if len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0], "limit") {
		t.Fatalf("download limit not reported: %v", v.Warnings)
	}
}

func TestVisualSessionGuards(t *testing.T) {
	b := New()
	if _, err := b.VisualEpisode(0, "episode"); err == nil {
		t.Fatal("accepted a missing session")
	}
	if _, err := b.VisualEpisodes(0); err == nil {
		t.Fatal("accepted a missing session")
	}
}

func visualTestMovie(count uint32, deltas []uint32) []byte {
	atom := func(name string, body []byte) []byte {
		out := make([]byte, 8+len(body))
		binary.BigEndian.PutUint32(out[:4], uint32(len(out)))
		copy(out[4:8], name)
		copy(out[8:], body)
		return out
	}
	header := make([]byte, 24)
	binary.BigEndian.PutUint32(header[12:16], 1_000_000)
	sizes := make([]byte, 12)
	binary.BigEndian.PutUint32(sizes[8:12], count)
	schedule := make([]byte, 8+8*len(deltas))
	binary.BigEndian.PutUint32(schedule[4:8], uint32(len(deltas)))
	for i, delta := range deltas {
		binary.BigEndian.PutUint32(schedule[8+i*8:12+i*8], 1)
		binary.BigEndian.PutUint32(schedule[12+i*8:16+i*8], delta)
	}
	return atom("moov", atom("trak", atom("mdia", append(atom("mdhd", header), atom("minf", atom("stbl", append(atom("stsz", sizes), atom("stts", schedule)...)))...))))
}

func TestVisualVideoScheduleRetainsSignedCameraOrigin(t *testing.T) {
	index := []byte("{\"canonical_episode_nanos\":-1000000000,\"segment\":\"segment.h264\",\"byte_size\":5}\n{\"canonical_episode_nanos\":-500000000,\"segment\":\"segment.h264\",\"byte_size\":5}\n{\"canonical_episode_nanos\":250000000,\"segment\":\"segment.h264\",\"byte_size\":5}\n")
	times, err := visualVideoTimes(index, visualTestMovie(3, []uint32{500000, 750000, 750000}))
	if err != nil {
		t.Fatal(err)
	}
	if len(times) != 3 || times[0] != -1000000000 || times[2] != 250000000 {
		t.Fatalf("video alignment changed: %v", times)
	}
	for name, movie := range map[string][]byte{
		"wrong count":    visualTestMovie(3, []uint32{500000, 750000}),
		"wrong schedule": visualTestMovie(3, []uint32{500000, 500000, 500000}),
		"truncated atom": {0, 0, 0, 64, 'm', 'o', 'o', 'v'},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := visualVideoTimes(index, movie); err == nil {
				t.Fatal("accepted unaligned video")
			}
		})
	}
}

func TestVisualVideoScheduleMatchesWendyRemux(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "cameras", "front")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	// Public H.264 SPS/PPS test vectors from episodeexport/playable_test.go.
	// These describe a 1280x720 Jetson bitstream, not authentication material.
	sps := []byte{0x67, 0x64, 0x00, 0x1f, 0xac, 0xb2, 0x00, 0xa0, 0x0b, 0x76, 0x02, 0xdc, 0x08, 0x08, 0x1a, 0x94, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xf2, 0x3c, 0x60, 0xc9, 0x20}
	pps := []byte{0x68, 0xeb, 0xcc, 0xb2, 0x2c}
	nal := func(parts ...[]byte) []byte {
		var raw []byte
		for _, p := range parts {
			raw = append(raw, 0, 0, 0, 1)
			raw = append(raw, p...)
		}
		return raw
	}
	frames := [][]byte{nal(sps, pps, append([]byte{0x65, 0x88}, make([]byte, 40)...)), nal(append([]byte{0x41, 0xc0}, make([]byte, 30)...)), nal(append([]byte{0x41, 0xc0}, make([]byte, 30)...))}
	var raw, index bytes.Buffer
	times := []int64{-100_000_000, -67_000_000, 28_000_000}
	for i, frame := range frames {
		row := map[string]any{"canonical_episode_nanos": times[i], "segment": "cameras/front/segment.h264", "byte_offset": raw.Len(), "byte_size": len(frame), "codec": "h264"}
		if err := json.NewEncoder(&index).Encode(row); err != nil {
			t.Fatal(err)
		}
		raw.Write(frame)
	}
	if err := os.WriteFile(filepath.Join(source, "index.jsonl"), index.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "segment.h264"), raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := episodeexport.ConvertSourceInPlace(root, source)
	if err != nil {
		t.Fatal(err)
	}
	movie, err := os.ReadFile(result.Output)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := visualVideoTimes(index.Bytes(), movie)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped) != 3 || mapped[0] != times[0] || mapped[2] != times[2] {
		t.Fatalf("real remux lost episode alignment: %v", mapped)
	}
}

func TestVisualLargeVideoStreamsToVerifiedOwnedFile(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	payload := bytes.Repeat([]byte{42}, visualFileLimit+1024)
	sum := sha256.Sum256(payload)
	meta := data.File{Path: "cameras/front/playable.mp4", Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}
	client := &visualFixtureClient{files: map[string][]byte{meta.Path: payload}}
	if _, err := visualDownload(context.Background(), client, "episode", meta); err == nil {
		t.Fatal("metadata cap was bypassed")
	}
	filename, err := visualDownloadVideo(context.Background(), client, "episode", meta)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filename)
	if err != nil || info.Size() != meta.Size || info.Mode().Perm() != 0600 {
		t.Fatalf("wrong file ownership/size: %v %v", info, err)
	}
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	_, err = io.Copy(digest, file)
	file.Close()
	if err != nil || !bytes.Equal(digest.Sum(nil), sum[:]) {
		t.Fatalf("file content mismatch: %v", err)
	}
	// The receiver can adopt the verified file before the bridge frees its old path.
	adopted := filepath.Join(temp, "adopted.mp4")
	if err := os.Rename(filename, adopted); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filename)
	if _, err := os.Stat(adopted); err != nil {
		t.Fatal("adopted file lost", err)
	}
	os.Remove(adopted)
	for name, mutate := range map[string]func(*pb.DataDownloadChunk){
		"corrupt": func(c *pb.DataDownloadChunk) { c.Data = bytes.Repeat([]byte{43}, len(c.Data)) },
		"offset": func(c *pb.DataDownloadChunk) {
			if c.Offset > 0 {
				c.Offset++
			}
		},
		"truncated": func(c *pb.DataDownloadChunk) { c.Eof = true },
	} {
		t.Run(name, func(t *testing.T) {
			client.mutate = mutate
			if _, err := visualDownloadVideo(context.Background(), client, "episode", meta); err == nil {
				t.Fatal("accepted invalid transfer")
			}
			files, err := os.ReadDir(temp)
			if err != nil || len(files) != 0 {
				t.Fatalf("partial video leaked: %v %v", files, err)
			}
		})
	}
	client.mutate = nil
	meta.Size = visualVideoFileLimit + 1
	if _, err := visualDownloadVideo(context.Background(), client, "episode", meta); err == nil {
		t.Fatal("accepted oversized video")
	}
	files, _ := os.ReadDir(temp)
	if len(files) != 0 {
		t.Fatal("oversized transfer leaked tempfile")
	}
}

type visualCountingReader struct {
	io.ReaderAt
	bytes int
}

func (r *visualCountingReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, off)
	r.bytes += n
	return n, err
}

func TestVisualVideoMetadataSeeksPastLargeMedia(t *testing.T) {
	index := []byte("{\"canonical_episode_nanos\":-1000,\"segment\":\"segment.h264\",\"byte_size\":5}\n")
	media := make([]byte, visualFileLimit+4096)
	binary.BigEndian.PutUint32(media[:4], uint32(len(media)))
	copy(media[4:8], "mdat")
	movie := append(media, visualTestMovie(1, []uint32{33000})...)
	reader := &visualCountingReader{ReaderAt: bytes.NewReader(movie)}
	times, err := visualVideoTimesReader(index, reader, int64(len(movie)))
	if err != nil || len(times) != 1 || times[0] != -1000 {
		t.Fatalf("large movie lost alignment: %v %v", times, err)
	}
	if reader.bytes > 4096 {
		t.Fatalf("loaded media data while seeking metadata: %d bytes", reader.bytes)
	}
	for _, invalid := range [][]byte{movie[:len(movie)-1], append(movie, visualTestMovie(1, []uint32{33000})...)} {
		if _, err := visualVideoTimesReader(index, bytes.NewReader(invalid), int64(len(invalid))); err == nil {
			t.Fatal("accepted invalid metadata")
		}
	}
}

func TestVisualRejectedVideoRemovesVerifiedTempfile(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	files := map[string][]byte{
		"cameras/front/index.jsonl":  []byte("{\"canonical_episode_nanos\":0,\"segment\":\"segment.h264\",\"byte_size\":5}\n"),
		"cameras/front/playable.mp4": visualTestMovie(2, []uint32{33000, 33000}),
	}
	manifest := data.Manifest{ID: "episode", State: "complete", CanonicalClock: "CLOCK_BOOTTIME", StoppedEpisodeNS: 1000}
	for name, raw := range files {
		sum := sha256.Sum256(raw)
		manifest.Files = append(manifest.Files, data.File{Path: name, Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:]), Role: data.FileRoleDerived})
	}
	snapshot, err := readVisualEpisode(context.Background(), &visualFixtureClient{manifest: manifest, files: files}, "episode")
	if err != nil || len(snapshot.Videos) != 0 || len(snapshot.Warnings) == 0 {
		t.Fatalf("invalid video result: %v %v", snapshot, err)
	}
	remaining, _ := os.ReadDir(temp)
	if len(remaining) != 0 {
		t.Fatal("unaligned movie leaked", remaining)
	}
}

func TestVisualEpisodeLoadsLargeVideoWithoutUsingMetadataBudget(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	media := make([]byte, visualFileLimit+4096)
	binary.BigEndian.PutUint32(media[:4], uint32(len(media)))
	copy(media[4:8], "mdat")
	files := map[string][]byte{
		"cameras/front/index.jsonl":  []byte("{\"canonical_episode_nanos\":0,\"segment\":\"segment.h264\",\"byte_size\":5}\n"),
		"cameras/front/playable.mp4": append(media, visualTestMovie(1, []uint32{33000})...),
	}
	manifest := data.Manifest{ID: "episode", State: "complete", CanonicalClock: "CLOCK_BOOTTIME", StoppedEpisodeNS: 1000}
	for name, raw := range files {
		sum := sha256.Sum256(raw)
		manifest.Files = append(manifest.Files, data.File{Path: name, Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:]), Role: data.FileRoleDerived, SourceID: "front"})
	}
	snapshot, err := readVisualEpisode(context.Background(), &visualFixtureClient{manifest: manifest, files: files}, "episode")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Videos) != 1 || len(snapshot.Warnings) != 0 {
		t.Fatalf("large video excluded: %+v", snapshot)
	}
	video := snapshot.Videos[0]
	defer os.Remove(video.Path)
	if video.Track != "front" || len(video.Times) != 1 || video.Times[0] != 0 {
		t.Fatalf("wrong frame alignment: %+v", video)
	}
	info, err := os.Stat(video.Path)
	if err != nil || info.Size() <= visualFileLimit {
		t.Fatalf("missing large owned video: %v %v", info, err)
	}
}

func TestVisualVideoFramesUseTheirFirstTransportChunkTimestamp(t *testing.T) {
	// Picture 2 begins in chunk 1 and extends into chunk 2. Pictures 3 and 4
	// both begin in chunk 3. Counts alone cannot establish frame alignment.
	index := []byte("{\"canonical_episode_nanos\":-1000000000,\"segment\":\"segment.h264\",\"byte_size\":5}\n" +
		"{\"canonical_episode_nanos\":-900000000,\"segment\":\"segment.h264\",\"byte_size\":5}\n" +
		"{\"canonical_episode_nanos\":-894191638,\"segment\":\"segment.h264\",\"byte_size\":5}\n" +
		"{\"canonical_episode_nanos\":-800000000,\"segment\":\"segment.h264\",\"byte_size\":5}\n")
	times, err := visualVideoTimes(index, visualTestMovie(5, []uint32{100000, 0, 100000, 0, 100000}))
	if err != nil {
		t.Fatal(err)
	}
	expected := []int64{-1000000000, -900000000, -900000000, -800000000, -800000000}
	if len(times) != len(expected) {
		t.Fatalf("wrong frame count %v", times)
	}
	for i, timestamp := range times {
		if timestamp != expected[i] {
			t.Fatalf("frame %d fabricated acquisition time: %d", i, timestamp)
		}
	}
	// A timestamp between recorded chunks has no evidentiary acquisition time.
	if _, err := visualVideoTimes(index, visualTestMovie(4, []uint32{100000, 50000, 50000, 100000})); err == nil {
		t.Fatal("accepted an unmatched media timestamp")
	}
}
