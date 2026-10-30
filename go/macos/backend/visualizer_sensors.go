package backend

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/foxglove/mcap/go/mcap"
	_ "github.com/mattn/go-sqlite3"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type VisualPoint struct{ X, Y, Z float32 }
type VisualAudio struct {
	Track, Path string
	Start, End  int64
}

const visualPointLimit = 4096
const visualPointTotalLimit = 1_000_000

// These files use the media budget, independent of camera/event metadata.
func (v *VisualSnapshot) readSensors(ctx context.Context, client pb.DataServiceClient, episode string, manifest data.Manifest) {
	budget := int64(visualVideoTotalLimit)
	for _, f := range manifest.Files {
		if ctx.Err() != nil {
			return
		}
		if strings.HasPrefix(f.Path, "audio/") && path.Base(f.Path) == "index.jsonl" {
			raw, err := visualDownload(ctx, client, episode, f)
			if err == nil {
				err = v.readAudio(ctx, client, episode, f, raw, manifest, &budget)
			}
			if err != nil {
				v.Warnings = append(v.Warnings, f.Path+": "+err.Error())
			}
		}
	}
	pointBudget := visualPointTotalLimit
	for _, f := range manifest.Files {
		if ctx.Err() != nil {
			return
		}
		if !strings.HasPrefix(f.Path, "ros2/") || (path.Ext(f.Path) != ".db3" && path.Ext(f.Path) != ".mcap") {
			continue
		}
		if f.Size < 0 || f.Size > visualVideoFileLimit || f.Size > budget {
			v.Warnings = append(v.Warnings, f.Path+": bag exceeds playback download limit; export the original episode")
			continue
		}
		clockPath := path.Dir(f.Path) + "-clock_samples.jsonl"
		var mappings []visualROSClock
		var err error
		for _, clockFile := range manifest.Files {
			if clockFile.Path == clockPath {
				var raw []byte
				raw, err = visualDownload(ctx, client, episode, clockFile)
				if err == nil {
					mappings, err = readROSClocks(raw)
				}
				break
			}
		}
		if err == nil && len(mappings) == 0 {
			err = errors.New("missing ROS storage clock mapping; export to inspect the original bag")
		}
		if err != nil {
			v.Warnings = append(v.Warnings, f.Path+": "+err.Error())
			continue
		}
		filename, err := visualDownloadVideo(ctx, client, episode, f)
		if err != nil {
			v.Warnings = append(v.Warnings, f.Path+": "+err.Error())
			continue
		}
		budget -= f.Size
		domain, _, ok := data.ParseROS2SourceID(f.SourceID)
		if !ok {
			err = errors.New("bag has no declared ROS 2 domain")
		} else {
			err = v.readROSBag(ctx, filename, path.Ext(f.Path), domain, mappings, &pointBudget)
		}
		_ = os.Remove(filename)
		if err != nil {
			v.Warnings = append(v.Warnings, f.Path+": "+err.Error())
		}
	}
}

type visualAudioIndex struct {
	Start       *int64 `json:"canonical_episode_nanos"`
	Uncertainty int64  `json:"canonical_uncertainty_nanos"`
	Segment     string `json:"segment"`
	Bytes       int64  `json:"byte_size"`
	Rate        uint32 `json:"sample_rate"`
	Channels    uint32 `json:"channels"`
	Duration    int64  `json:"duration_nanos"`
}

func (v *VisualSnapshot) readAudio(ctx context.Context, client pb.DataServiceClient, episode string, index data.File, raw []byte, manifest data.Manifest, budget *int64) error {
	files := map[string]data.File{}
	for _, f := range manifest.Files {
		files[f.Path] = f
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var row visualAudioIndex
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return err
		}
		f, ok := files[row.Segment]
		if index.SourceID == "" || !ok || row.Uncertainty < 0 || row.Start == nil || row.Duration <= 0 || *row.Start > math.MaxInt64-row.Duration || path.Dir(row.Segment) != path.Dir(index.Path) {
			return errors.New("invalid audio segment index")
		}
		if f.Size < 0 || f.Size > visualVideoFileLimit || f.Size > *budget {
			return errors.New("audio exceeds playback download limit; export the original episode")
		}
		filename, err := visualDownloadVideo(ctx, client, episode, f)
		if err != nil {
			return err
		}
		*budget -= f.Size
		peaks, err := visualWAVPeaks(filename, row)
		if err != nil {
			_ = os.Remove(filename)
			return err
		}
		v.Audio = append(v.Audio, VisualAudio{Track: index.SourceID, Path: filename, Start: *row.Start, End: *row.Start + row.Duration})
		v.Start = min(v.Start, *row.Start)
		v.End = max(v.End, *row.Start+row.Duration)
		v.track(index.SourceID, index.SourceID, "audio", "Recorded PCM audio")
		for i, peak := range peaks {
			v.sample(VisualSample{Track: index.SourceID, Time: *row.Start + int64(i)*50_000_000, Label: "Audio", Value: peak, HasValue: true, Unit: "peak", Detail: fmt.Sprintf("Audio segment · clock uncertainty ±%g ms", float64(row.Uncertainty)/1e6)})
		}
	}
	return scanner.Err()
}

// The recorder emits PCM s16le WAV. Verify the schedule against the actual
// header/payload before letting a player align it with the episode cursor.
func visualWAVPeaks(filename string, row visualAudioIndex) ([]float64, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var h [44]byte
	if _, err = io.ReadFull(f, h[:]); err != nil {
		return nil, err
	}
	rate := binary.LittleEndian.Uint32(h[24:])
	channels := binary.LittleEndian.Uint16(h[22:])
	n := binary.LittleEndian.Uint32(h[40:])
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if string(h[:4]) != "RIFF" || string(h[8:16]) != "WAVEfmt " || binary.LittleEndian.Uint32(h[16:]) != 16 || binary.LittleEndian.Uint16(h[20:]) != 1 || binary.LittleEndian.Uint16(h[34:]) != 16 || string(h[36:40]) != "data" || binary.LittleEndian.Uint32(h[4:]) != n+36 || binary.LittleEndian.Uint32(h[28:]) != rate*uint32(channels)*2 || binary.LittleEndian.Uint16(h[32:]) != channels*2 || rate == 0 || rate > 384000 || rate%20 != 0 || channels == 0 || channels > 8 || rate != row.Rate || uint32(channels) != row.Channels || int64(n) != row.Bytes || info.Size() != int64(n)+44 || n%uint32(channels*2) != 0 || int64(n)*1e9/int64(rate)/int64(channels*2) != row.Duration {
		return nil, errors.New("audio header or duration does not match its index")
	}
	block := make([]byte, int(rate/20)*int(channels)*2)
	if len(block) == 0 {
		return nil, errors.New("unsupported sample rate")
	}
	var peaks []float64
	for {
		n, err := io.ReadFull(f, block)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, err
		}
		if n == 0 {
			break
		}
		peak := 0.0
		for i := 0; i+1 < n; i += 2 {
			peak = math.Max(peak, math.Abs(float64(int16(binary.LittleEndian.Uint16(block[i:]))))/32768)
		}
		peaks = append(peaks, peak)
		if len(peaks) > visualSampleLimit {
			return nil, errors.New("audio waveform exceeds viewer sample limit")
		}
		if err != nil {
			break
		}
	}
	return peaks, nil
}

type visualROSClock struct {
	Episode int64            `json:"episode_nanos"`
	Kind    string           `json:"kind"`
	Sample  data.ClockSample `json:"sample"`
}

func readROSClocks(raw []byte) ([]visualROSClock, error) {
	var out []visualROSClock
	s := bufio.NewScanner(bytes.NewReader(raw))
	s.Buffer(make([]byte, 4096), 1<<20)
	for s.Scan() {
		var row visualROSClock
		if err := json.Unmarshal(s.Bytes(), &row); err != nil {
			return nil, err
		}
		if row.Kind != "host_realtime_sandwich" {
			continue
		}
		if row.Sample.BootAfterNanos < row.Sample.BootBeforeNanos || row.Sample.TargetNanos <= 0 {
			return nil, errors.New("invalid ROS clock bracket")
		}
		if len(out) > 0 {
			last := out[len(out)-1]
			drift := (float64(row.Sample.TargetNanos) - float64(last.Sample.TargetNanos)) - (float64(row.Episode) - float64(last.Episode))
			if row.Episode <= last.Episode || math.Abs(drift) > 50e6 {
				return nil, errors.New("ROS storage clock discontinuity; synchronized replay unavailable")
			}
		}
		out = append(out, row)
	}
	return out, s.Err()
}
func rosEpisodeTime(stamp int64, clocks []visualROSClock) (int64, error) {
	if len(clocks) == 0 {
		return 0, errors.New("no ROS clock mapping")
	}
	i := sort.Search(len(clocks), func(i int) bool { return clocks[i].Sample.TargetNanos >= stamp })
	if i == len(clocks) {
		i--
	}
	if i > 0 && math.Abs(float64(stamp)-float64(clocks[i-1].Sample.TargetNanos)) < math.Abs(float64(stamp)-float64(clocks[i].Sample.TargetNanos)) {
		i--
	}
	c := clocks[i]
	delta := float64(stamp) - float64(c.Sample.TargetNanos)
	if math.Abs(delta) > 2e9 {
		return 0, errors.New("ROS message is outside the measured clock mapping")
	}
	d := stamp - c.Sample.TargetNanos
	if (d > 0 && c.Episode > math.MaxInt64-d) || (d < 0 && c.Episode < math.MinInt64-d) {
		return 0, errors.New("ROS timestamp overflow")
	}
	return c.Episode + d, nil
}
func (v *VisualSnapshot) readROSBag(ctx context.Context, filename, format, domain string, clocks []visualROSClock, pointBudget *int) error {
	count := 0
	consume := func(topic, typ, encoding string, stamp int64, payload []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > visualSampleLimit || len(v.Samples) >= visualSampleLimit {
			return errors.New("ROS viewer sample limit reached; export the original episode for all messages")
		}
		t, err := rosEpisodeTime(stamp, clocks)
		if err != nil {
			return err
		}
		id := domain + ":" + topic
		sample := VisualSample{Track: id, Time: t, Label: typ, Detail: fmt.Sprintf("%s · %d bytes\nMapped bag receipt time; message header time remains in the original bag.", typ, len(payload))}
		kind := "ros2"
		if encoding == "cdr" {
			decoded, frame, err := decodeVisualROS(typ, payload)
			if err != nil {
				sample.Detail += "\n" + err.Error()
			} else {
				sample.Detail += "\n" + frame
				if typ == "sensor_msgs/msg/PointCloud2" || typ == "sensor_msgs/msg/LaserScan" {
					if len(decoded.Points) > *pointBudget {
						return errors.New("LiDAR preview point limit reached; export the original bag for all points")
					}
					*pointBudget -= len(decoded.Points)
					sample.Points = decoded.Points
					kind = "lidar"
					sample.Detail += "\nAt most 4,096 points per preview; original points retained in export. Sensor frame, no TF transforms."
				}
				for _, value := range decoded.Values {
					track := id + "/" + value.Name
					v.track(track, topic+" · "+value.Name, "numeric", typ)
					v.sample(VisualSample{Track: track, Time: t, Value: value.Value, HasValue: true, Unit: value.Unit, Detail: sample.Detail})
				}
			}
		} else {
			sample.Detail += "\nUnsupported message encoding; original payload available in export."
		}
		v.track(id, topic, kind, typ)
		// Manifest topic entries already exist; promote them when a decoder succeeds.
		if kind == "lidar" {
			for i := range v.Tracks {
				if v.Tracks[i].ID == id {
					v.Tracks[i].Kind = kind
					v.Tracks[i].Name = topic
				}
			}
		}
		v.sample(sample)
		return nil
	}
	if format == ".db3" {
		u := url.URL{Scheme: "file", Path: filename}
		q := u.Query()
		q.Set("mode", "ro")
		q.Set("immutable", "1")
		u.RawQuery = q.Encode()
		db, err := sql.Open("sqlite3", u.String())
		if err != nil {
			return err
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		rows, err := db.QueryContext(ctx, `SELECT t.name,t.type,t.serialization_format,m.timestamp,length(m.data),CASE WHEN length(m.data)<=16777216 THEN m.data ELSE NULL END FROM messages m JOIN topics t ON t.id=m.topic_id ORDER BY m.timestamp LIMIT 20001`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var topic, typ, encoding string
			var stamp int64
			var payload []byte
			var size int64
			if err := rows.Scan(&topic, &typ, &encoding, &stamp, &size, &payload); err != nil {
				return err
			}
			if size > visualFileLimit {
				return errors.New("ROS message exceeds preview limit; export the original bag")
			}
			if err := consume(topic, typ, encoding, stamp, payload); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	lexer, err := mcap.NewLexer(f, &mcap.LexerOptions{ValidateChunkCRCs: true, MaxDecompressedChunkSize: 64 << 20, MaxRecordSize: 64 << 20})
	if err != nil {
		return err
	}
	defer lexer.Close()
	schemas := map[uint16]string{}
	channels := map[uint16]*mcap.Channel{}
	for {
		token, raw, err := lexer.Next(nil)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch token {
		case mcap.TokenSchema:
			s, err := mcap.ParseSchema(raw)
			if err != nil {
				return err
			}
			schemas[s.ID] = s.Name
		case mcap.TokenChannel:
			c, err := mcap.ParseChannel(raw)
			if err != nil {
				return err
			}
			channels[c.ID] = c
		case mcap.TokenMessage:
			m, err := mcap.ParseMessage(raw)
			if err != nil {
				return err
			}
			c := channels[m.ChannelID]
			if c == nil || m.LogTime > math.MaxInt64 {
				return errors.New("invalid MCAP message metadata")
			}
			if len(m.Data) > visualFileLimit {
				return errors.New("ROS message exceeds preview limit")
			}
			if err := consume(c.Topic, schemas[c.SchemaID], c.MessageEncoding, int64(m.LogTime), m.Data); err != nil {
				return err
			}
		}
	}
}
