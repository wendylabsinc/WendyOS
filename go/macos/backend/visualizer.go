package backend

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	recording "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/protobuf/proto"
)

const visualFileLimit = 16 << 20
const visualTotalLimit = 64 << 20
const visualSampleLimit = 20000
const visualVideoFileLimit = 512 << 20
const visualVideoTotalLimit = 1 << 30

type VisualTrack struct{ ID, Name, Kind, Detail string }
type VisualSample struct {
	Track, Label, Detail, Unit string
	Time                       int64
	Value                      float64
	HasValue                   bool
	Image                      []byte
}
type VisualSnapshot struct {
	Clock, BootID string
	Start, End    int64
	Tracks        []VisualTrack
	Samples       []VisualSample
	Warnings      []string
	Videos        []VisualVideo
}

type VisualVideo struct {
	Track string
	Path  string
	Times []int64
}

func (b *Backend) VisualEpisodes(session uint64) ([]*pb.DataEpisode, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return nil, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	r, err := conn.DataService.Episodes(ctx, &pb.DataEpisodesRequest{})
	if err != nil {
		return nil, err
	}
	return r.GetEpisodes(), nil
}

func (b *Backend) VisualEpisode(session uint64, episode string) (*VisualSnapshot, error) {
	b.mu.Lock()
	if session == 0 || episode == "" {
		b.mu.Unlock()
		return nil, errors.New("choose an episode on the connected device")
	}
	conn, err := b.connection(session)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	// Downloads use the captured connection and end when that session closes.
	// They must not hold the connection lock while a user changes devices.
	life := b.life
	if life == nil {
		life = context.Background()
	}
	ctx, cancel := context.WithTimeout(life, 5*time.Minute)
	client := conn.DataService
	b.mu.Unlock()
	defer cancel()
	return readVisualEpisode(ctx, client, episode)
}

func readVisualEpisode(ctx context.Context, client pb.DataServiceClient, episode string) (*VisualSnapshot, error) {
	r, err := client.Inspect(ctx, &pb.DataInspectRequest{Episode: episode})
	if err != nil {
		return nil, err
	}
	if len(r.GetManifestJson()) > visualFileLimit {
		return nil, errors.New("episode manifest exceeds the viewer limit")
	}
	var manifest data.Manifest
	if err := json.Unmarshal(r.GetManifestJson(), &manifest); err != nil {
		return nil, fmt.Errorf("read episode manifest: %w", err)
	}
	if manifest.ID != episode {
		return nil, errors.New("the Agent returned a different episode")
	}
	if manifest.CanonicalClock != "CLOCK_BOOTTIME" {
		return nil, errors.New("this episode does not declare a supported canonical clock")
	}
	out := &VisualSnapshot{Clock: manifest.CanonicalClock, BootID: manifest.BootID, Start: manifest.StartedEpisodeNS, End: manifest.StoppedEpisodeNS}
	complete := false
	defer func() {
		if !complete {
			for _, video := range out.Videos {
				_ = os.Remove(video.Path)
			}
		}
	}()
	if out.BootID == "" {
		out.BootID = manifest.Device.BootID
	}
	if manifest.State != "complete" && manifest.State != "interrupted" {
		out.Warnings = append(out.Warnings, "Only sealed files are shown. Refresh after recording finishes to load the complete episode.")
	}
	for _, source := range manifest.Sources {
		detail := source.Source.Detail
		if source.Drops == nil {
			detail += " · Dropped samples unknown"
		} else if *source.Drops > 0 {
			detail += fmt.Sprintf(" · %d dropped samples", *source.Drops)
		}
		if source.MappingError != nil {
			detail += fmt.Sprintf(" · Clock mapping ±%g ms", float64(*source.MappingError)/1e6)
		}
		out.track(source.Source.ID, source.Source.ID, source.Source.Kind, detail)
	}
	out.Warnings = append(out.Warnings, manifest.PlayableNotes...)
	budget := int64(visualTotalLimit)
	indexes := map[string][]byte{}
	for _, file := range manifest.Files {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if file.Path != "events.jsonl" && file.Path != "telemetry.jsonl" && file.Path != data.RecordingLogFile && !(strings.HasPrefix(file.Path, "cameras/") && path.Base(file.Path) == "index.jsonl") {
			continue
		}
		if file.Size < 0 || file.Size > visualFileLimit || file.Size > budget {
			out.Warnings = append(out.Warnings, file.Path+": exceeds the viewer download limit; use Wendy Data export for the full recording")
			continue
		}
		payload, err := visualDownload(ctx, client, episode, file)
		if err != nil {
			out.Warnings = append(out.Warnings, file.Path+": "+err.Error())
			continue
		}
		budget -= int64(len(payload))
		if strings.HasPrefix(file.Path, "cameras/") && path.Base(file.Path) == "index.jsonl" {
			indexes[file.Path] = payload
		}
		if file.Path == data.RecordingLogFile {
			err = out.readRecords(payload, manifest)
		} else {
			err = out.readJSONLines(payload, file)
		}
		if err != nil {
			out.Warnings = append(out.Warnings, file.Path+": "+err.Error())
		}
		if len(out.Samples) >= visualSampleLimit {
			out.Warnings = append(out.Warnings, "Showing the first 20,000 samples read. Export the episode for the complete data.")
			break
		}
	}
	videoBudget := int64(visualVideoTotalLimit)
	for _, file := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if file.Role != data.FileRoleDerived || path.Base(file.Path) != "playable.mp4" || !strings.HasPrefix(file.Path, "cameras/") {
			continue
		}
		index, exists := indexes[path.Join(path.Dir(file.Path), "index.jsonl")]
		if !exists {
			continue
		}
		if file.Size < 0 || file.Size > visualVideoFileLimit || file.Size > videoBudget {
			out.Warnings = append(out.Warnings, file.Path+": video exceeds the 512 MiB clip or 1 GiB episode playback limit")
			continue
		}
		movie, err := visualDownloadVideo(ctx, client, episode, file)
		if err != nil {
			out.Warnings = append(out.Warnings, file.Path+": "+err.Error())
			continue
		}
		videoBudget -= file.Size
		times, err := visualVideoTimesFile(index, movie)
		if err != nil {
			_ = os.Remove(movie)
			out.Warnings = append(out.Warnings, file.Path+": "+err.Error())
			continue
		}
		track := file.SourceID
		if track == "" {
			track = path.Dir(file.Path)
		}
		out.Videos = append(out.Videos, VisualVideo{Track: track, Path: movie, Times: times})
	}
	for _, s := range out.Samples {
		out.Start = min(out.Start, s.Time)
		out.End = max(out.End, s.Time)
	}
	if out.End <= out.Start {
		if out.Start == math.MaxInt64 {
			return nil, errors.New("episode timeline exceeds the supported range")
		}
		out.End = out.Start + 1
	}
	sort.SliceStable(out.Samples, func(i, j int) bool { return out.Samples[i].Time < out.Samples[j].Time })
	sort.Slice(out.Tracks, func(i, j int) bool { return out.Tracks[i].Name < out.Tracks[j].Name })
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	complete = true
	return out, nil
}

// Downloads are checked against the manifest before any payload is decoded.
// No untrusted relative path is used as a filesystem path.
func visualDownload(ctx context.Context, client pb.DataServiceClient, episode string, file data.File) ([]byte, error) {
	var body bytes.Buffer
	if err := visualDownloadTo(ctx, client, episode, file, visualFileLimit, &body); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}

// The caller owns the returned file. Failed transfers remove their partial file.
func visualDownloadVideo(ctx context.Context, client pb.DataServiceClient, episode string, file data.File) (result string, err error) {
	f, err := os.CreateTemp("", "wendy-data-video-*.mp4")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	if err = visualDownloadTo(ctx, client, episode, file, visualVideoFileLimit, f); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func visualDownloadTo(ctx context.Context, client pb.DataServiceClient, episode string, file data.File, limit int64, target io.Writer) error {
	if file.Size < 0 || file.Size > limit || file.Path == "" || path.Clean(file.Path) != file.Path || strings.HasPrefix(file.Path, "/") || strings.HasPrefix(file.Path, "../") {
		return errors.New("invalid episode file metadata")
	}
	want, err := hex.DecodeString(file.SHA256)
	if err != nil || len(want) != sha256.Size {
		return errors.New("file has no valid checksum")
	}
	s, err := client.Download(ctx, &pb.DataDownloadRequest{Episode: episode, Path: file.Path})
	if err != nil {
		return err
	}
	digest := sha256.New()
	writer := io.MultiWriter(target, digest)
	var received int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if chunk.GetPath() != file.Path || chunk.GetOffset() != received || chunk.GetSize() != file.Size || chunk.GetSha256() != file.SHA256 || int64(len(chunk.GetData())) > file.Size-received {
			return errors.New("download metadata or offsets do not match the manifest")
		}
		n, err := writer.Write(chunk.GetData())
		if err != nil {
			return err
		}
		received += int64(n)
		if chunk.GetEof() {
			break
		}
	}
	if received != file.Size {
		return errors.New("download ended before the complete file arrived")
	}
	if !bytes.Equal(digest.Sum(nil), want) {
		return errors.New("download checksum mismatch")
	}
	return nil
}

// Skip media atoms with positional reads; only the bounded timing metadata is
// loaded into memory. A large mdat never crosses the Go/C/Swift bridge.
func visualVideoTimesFile(index []byte, filename string) ([]int64, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return visualVideoTimesReader(index, f, info.Size())
}

func visualVideoTimesReader(index []byte, movie io.ReaderAt, length int64) ([]int64, error) {
	var metadata []byte
	atoms := 0
	for offset := int64(0); offset < length; {
		atoms++
		if atoms > 8192 {
			return nil, errors.New("video atom count exceeds viewer limit")
		}
		var header [16]byte
		if length-offset < 8 {
			return nil, errors.New("truncated video atom")
		}
		if _, err := movie.ReadAt(header[:8], offset); err != nil {
			return nil, err
		}
		size, headerSize := uint64(binary.BigEndian.Uint32(header[:4])), uint64(8)
		if size == 1 {
			if length-offset < 16 {
				return nil, errors.New("truncated video atom")
			}
			if _, err := movie.ReadAt(header[8:], offset+8); err != nil {
				return nil, err
			}
			size, headerSize = binary.BigEndian.Uint64(header[8:]), 16
		}
		if size == 0 {
			size = uint64(length - offset)
		}
		if size < headerSize || size > uint64(length-offset) {
			return nil, errors.New("invalid video atom size")
		}
		if string(header[4:8]) == "moov" {
			if metadata != nil {
				return nil, errors.New("multiple video metadata atoms")
			}
			if size > visualFileLimit {
				return nil, errors.New("video timing metadata exceeds the viewer limit")
			}
			metadata = make([]byte, int(size))
			if _, err := movie.ReadAt(metadata, offset); err != nil {
				return nil, err
			}
		}
		offset += int64(size)
	}
	if metadata == nil {
		return nil, errors.New("video has no timing metadata")
	}
	return visualVideoTimes(index, metadata)
}

func (v *VisualSnapshot) track(id, name, kind, detail string) {
	for _, t := range v.Tracks {
		if t.ID == id {
			return
		}
	}
	v.Tracks = append(v.Tracks, VisualTrack{ID: id, Name: name, Kind: kind, Detail: detail})
}
func (v *VisualSnapshot) sample(s VisualSample) {
	if len(v.Samples) >= visualSampleLimit || (s.HasValue && (math.IsNaN(s.Value) || math.IsInf(s.Value, 0))) {
		return
	}
	v.Samples = append(v.Samples, s)
}

func (v *VisualSnapshot) readJSONLines(raw []byte, file data.File) error {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		if len(v.Samples) >= visualSampleLimit {
			break
		}
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var row struct {
			Time              *int64 `json:"episode_nanos"`
			Canonical         *int64 `json:"canonical_episode_nanos"`
			Sample            uint64 `json:"sample_id"`
			Uncertainty       int64  `json:"canonical_uncertainty_nanos"`
			Type, Name, Model string
			App               string                     `json:"app_id"`
			Accepted          bool                       `json:"client_timestamp_accepted"`
			Values            map[string]json.RawMessage `json:"values"`
			Attributes        map[string]json.RawMessage `json:"attributes"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return fmt.Errorf("invalid sample: %w", err)
		}
		if row.Canonical != nil {
			track := file.SourceID
			if track == "" {
				track = path.Dir(file.Path)
			}
			v.track(track, track, "camera", "Recorded camera frame/chunk timestamps.")
			v.sample(VisualSample{Track: track, Time: *row.Canonical, Label: fmt.Sprintf("Frame %d", row.Sample), Detail: fmt.Sprintf("Recorded frame/chunk timestamp · ±%g ms", float64(row.Uncertainty)/1e6)})
			continue
		}
		if row.Time == nil {
			return errors.New("sample has no canonical episode timestamp")
		}
		if file.Path == "telemetry.jsonl" {
			keys := make([]string, 0, len(row.Values))
			for key := range row.Values {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				var value float64
				if json.Unmarshal(row.Values[key], &value) != nil {
					continue
				}
				unit := ""
				if strings.HasSuffix(key, "_bytes") {
					unit = "bytes"
				}
				if strings.HasSuffix(key, "_seconds") {
					unit = "s"
				}
				track := "telemetry/" + key
				v.track(track, strings.ReplaceAll(key, "_", " "), "numeric", "Device telemetry acquisition time")
				v.sample(VisualSample{Track: track, Time: *row.Time, Value: value, HasValue: true, Unit: unit})
			}
			continue
		}
		track := "app/" + row.App
		v.track(track, row.App, "events", "Application events and predictions")
		label := row.Name
		if label == "" {
			label = row.Model
		}
		if label == "" {
			label = row.Type
		}
		basis := "Device receipt timestamp"
		if row.Accepted {
			basis = "Accepted application timestamp"
		}
		var details []string
		keys := make([]string, 0, len(row.Attributes))
		for key := range row.Attributes {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			details = append(details, key+": "+string(row.Attributes[key]))
		}
		v.sample(VisualSample{Track: track, Time: *row.Time, Label: label, Detail: basis + "\n" + strings.Join(details, "\n")})
	}
	return scanner.Err()
}

func (v *VisualSnapshot) readRecords(raw []byte, manifest data.Manifest) error {
	r := bytes.NewReader(raw)
	for r.Len() > 0 && len(v.Samples) < visualSampleLimit {
		record, _, err := data.ReadRecording(r)
		if err != nil {
			return err
		}
		if record.GetRecord() == nil || record.GetStreamDescriptor() == nil {
			return errors.New("record has no payload descriptor")
		}
		id := strings.Join([]string{record.GetAppId(), record.GetService(), record.GetStream()}, "/")
		desc := record.GetStreamDescriptor()
		payload := record.GetRecord().GetPayload()
		if desc.GetSchema() == "wendy.agent.apps.v1.TimeSeriesBatch" {
			var batch recording.TimeSeriesBatch
			if err := proto.Unmarshal(payload, &batch); err != nil {
				return err
			}
			times, err := visualTimes(batch.GetTiming(), manifest)
			if err != nil {
				v.Warnings = append(v.Warnings, id+": "+err.Error())
				continue
			}
			if len(batch.GetColumns()) != len(desc.GetChannels()) {
				return errors.New("time-series columns do not match the descriptor")
			}
			for column, channel := range desc.GetChannels() {
				values := visualValues(batch.GetColumns()[column])
				if len(values) != len(times) {
					return errors.New("time-series value count does not match timestamps")
				}
				track := id + "/" + channel.GetName()
				v.track(track, channel.GetName(), "numeric", id+" · Acquisition timestamps")
				for i, value := range values {
					v.sample(VisualSample{Track: track, Time: times[i], Value: value, HasValue: true, Unit: channel.GetUnit()})
				}
			}
			continue
		}
		kind, label := "events", record.GetRecord().GetId()
		basis := "Device receipt timestamp"
		if record.GetClientTimestampAccepted() {
			basis = "Accepted application timestamp"
		}
		sample := VisualSample{Track: id, Time: record.GetEpisodeNanos(), Label: label, Detail: basis + " · " + desc.GetMediaType()}
		if (desc.GetMediaType() == "image/jpeg" || desc.GetMediaType() == "image/png") && len(payload) <= 2<<20 {
			kind = "image"
			sample.Image = bytes.Clone(payload)
		} else if strings.HasPrefix(desc.GetMediaType(), "text/") || desc.GetMediaType() == "application/json" {
			sample.Detail += "\n" + string(payload[:min(len(payload), 4096)])
		} else {
			sample.Detail += fmt.Sprintf(" · %d bytes; no payload decoder", len(payload))
		}
		v.track(id, record.GetStream(), kind, record.GetAppId()+" · "+record.GetService())
		v.sample(sample)
	}
	return nil
}

func visualTimes(t *recording.SampleTiming, m data.Manifest) ([]int64, error) {
	boot := m.BootID
	if boot == "" {
		boot = m.Device.BootID
	}
	if t == nil || t.GetClock() != "CLOCK_BOOTTIME" || boot == "" || boot == "unavailable" || t.GetBootId() != boot {
		return nil, errors.New("sample clock cannot be aligned with this episode; acquisition samples omitted")
	}
	if t.GetCount() == 0 || t.GetCount() > visualSampleLimit {
		return nil, errors.New("sample batch exceeds viewer limit")
	}
	var times []int64
	if len(t.GetTimestampsNanos()) > 0 {
		if len(t.GetTimestampsNanos()) != int(t.GetCount()) {
			return nil, errors.New("timestamp count does not match the batch")
		}
		times = append(times, t.GetTimestampsNanos()...)
	} else {
		if t.StartNanos == nil || t.PeriodNanos == nil || t.GetStartNanos() < 0 || t.GetPeriodNanos() <= 0 {
			return nil, errors.New("sample batch has no valid timing")
		}
		if int64(t.GetCount()-1) > (math.MaxInt64-t.GetStartNanos())/t.GetPeriodNanos() {
			return nil, errors.New("sample timing overflow")
		}
		for i := uint32(0); i < t.GetCount(); i++ {
			times = append(times, t.GetStartNanos()+int64(i)*t.GetPeriodNanos())
		}
	}
	for i := range times {
		if times[i] < 0 || m.RequestBootNanos < 0 {
			return nil, errors.New("invalid boottime timestamp")
		}
		times[i] -= m.RequestBootNanos
	}
	return times, nil
}

func visualValues(c *recording.SampleColumn) []float64 {
	var out []float64
	switch v := c.GetValues().(type) {
	case *recording.SampleColumn_Float32Values:
		for _, value := range v.Float32Values.GetValues() {
			out = append(out, float64(value))
		}
	case *recording.SampleColumn_Float64Values:
		out = append(out, v.Float64Values.GetValues()...)
	case *recording.SampleColumn_IntValues:
		for _, value := range v.IntValues.GetValues() {
			out = append(out, float64(value))
		}
	case *recording.SampleColumn_UintValues:
		for _, value := range v.UintValues.GetValues() {
			out = append(out, float64(value))
		}
	case *recording.SampleColumn_BoolValues:
		for _, value := range v.BoolValues.GetValues() {
			if value {
				out = append(out, 1)
			} else {
				out = append(out, 0)
			}
		}
	}
	return out
}

// Wendy's sealed MP4 stores one sample per assembled picture, while the index
// stores transport chunks. Several pictures can begin in one chunk, and a
// picture can span several chunks. The muxer uses the first indexed chunk as
// its clock base and each picture's first chunk as its acquisition timestamp.
// Match every media timestamp to that ordered index; never assume frame N is
// index row N, or interpolate acquisition times from a nominal frame rate.
func visualVideoTimes(index, movie []byte) ([]int64, error) {
	var times []int64
	scan := bufio.NewScanner(bytes.NewReader(index))
	scan.Buffer(make([]byte, 4096), 2<<20)
	for scan.Scan() {
		if len(bytes.TrimSpace(scan.Bytes())) == 0 {
			continue
		}
		var row struct {
			Time    *int64 `json:"canonical_episode_nanos"`
			Segment string `json:"segment"`
			Size    int64  `json:"byte_size"`
		}
		if err := json.Unmarshal(scan.Bytes(), &row); err != nil || row.Time == nil || row.Size <= 0 || !strings.HasSuffix(strings.ToLower(row.Segment), ".h264") {
			return nil, errors.New("camera index cannot be mapped to video frames")
		}
		if len(times) >= visualSampleLimit {
			return nil, errors.New("video timeline exceeds the viewer sample limit")
		}
		if len(times) > 0 && *row.Time < times[len(times)-1] {
			return nil, errors.New("video has inverted timestamps; showing its index only")
		}
		times = append(times, *row.Time)
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	if len(times) == 0 {
		return nil, errors.New("video has no recorded frame timestamps")
	}
	tables := map[string][]byte{}
	if err := visualMovieTables(movie, tables, 0); err != nil {
		return nil, err
	}
	sizes, schedule, header := tables["stsz"], tables["stts"], tables["mdhd"]
	var scale uint32
	if len(header) >= 16 && header[0] == 0 {
		scale = binary.BigEndian.Uint32(header[12:16])
	}
	if len(header) >= 24 && header[0] == 1 {
		scale = binary.BigEndian.Uint32(header[20:24])
	}
	if len(sizes) < 12 || len(schedule) < 8 || scale != 1_000_000 {
		return nil, errors.New("video does not carry the expected Wendy timestamp schedule")
	}
	sampleCount := int(binary.BigEndian.Uint32(sizes[8:12]))
	if sampleCount == 0 || sampleCount > visualSampleLimit {
		return nil, errors.New("video sample count exceeds the supported range")
	}
	// Quantization is the same as episodeexport: nearest microsecond relative
	// to the first indexed chunk. Keep the original nanoseconds for the UI.
	ticks := make([]int64, len(times))
	for i, timestamp := range times {
		if times[0] < 0 && timestamp > math.MaxInt64+times[0]-500 {
			return nil, errors.New("video timeline overflow")
		}
		relative := timestamp - times[0]
		if relative > math.MaxInt64-500 {
			return nil, errors.New("video timeline overflow")
		}
		ticks[i] = (relative + 500) / 1000
	}
	entries := int(binary.BigEndian.Uint32(schedule[4:8]))
	if entries > (len(schedule)-8)/8 {
		return nil, errors.New("truncated video timestamp table")
	}
	var mediaTime int64
	var frames []int64
	chunk := 0
	for i := 0; i < entries; i++ {
		count := int(binary.BigEndian.Uint32(schedule[8+i*8 : 12+i*8]))
		delta := int64(binary.BigEndian.Uint32(schedule[12+i*8 : 16+i*8]))
		if count > sampleCount-len(frames) {
			return nil, errors.New("video timestamp count disagrees with its sample table")
		}
		for j := 0; j < count; j++ {
			for chunk < len(ticks) && ticks[chunk] < mediaTime {
				chunk++
			}
			if chunk == len(ticks) || ticks[chunk] != mediaTime {
				return nil, errors.New("video timestamps disagree with the capture index; showing its index only")
			}
			// Zero-duration samples intentionally reuse this chunk's recorded time.
			frames = append(frames, times[chunk])
			mediaTime += delta
		}
	}
	if len(frames) != sampleCount {
		return nil, errors.New("incomplete video timestamp schedule")
	}
	return frames, nil
}

func visualMovieTables(raw []byte, tables map[string][]byte, depth int) error {
	if depth > 8 {
		return errors.New("video atom nesting exceeds viewer limit")
	}
	for len(raw) > 0 {
		if len(raw) < 8 {
			return errors.New("truncated video atom")
		}
		size := uint64(binary.BigEndian.Uint32(raw[:4]))
		name := string(raw[4:8])
		header := 8
		if size == 1 {
			if len(raw) < 16 {
				return errors.New("truncated video atom")
			}
			size = binary.BigEndian.Uint64(raw[8:16])
			header = 16
		}
		if size == 0 {
			size = uint64(len(raw))
		}
		if size < uint64(header) || size > uint64(len(raw)) {
			return errors.New("invalid video atom size")
		}
		body := raw[header:int(size)]
		switch name {
		case "moov", "trak", "mdia", "minf", "stbl":
			if err := visualMovieTables(body, tables, depth+1); err != nil {
				return err
			}
		case "mdhd", "stts", "stsz":
			if _, exists := tables[name]; exists {
				return errors.New("multiple video tracks are not supported")
			}
			tables[name] = body
		}
		raw = raw[int(size):]
	}
	return nil
}
