package backend

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/foxglove/mcap/go/mcap"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type cdrFixture struct {
	raw   []byte
	order binary.ByteOrder
}

func newCDRFixture(big bool) *cdrFixture {
	w := &cdrFixture{raw: []byte{0, 1, 0, 0}, order: binary.LittleEndian}
	if big {
		w.raw[1] = 0
		w.order = binary.BigEndian
	}
	return w
}
func (w *cdrFixture) align(n int) {
	for (len(w.raw)-4)%n != 0 {
		w.raw = append(w.raw, 0)
	}
}
func (w *cdrFixture) u32(n uint32) {
	w.align(4)
	var b [4]byte
	w.order.PutUint32(b[:], n)
	w.raw = append(w.raw, b[:]...)
}
func (w *cdrFixture) f32(n float32) { w.u32(math.Float32bits(n)) }
func (w *cdrFixture) str(s string) {
	w.u32(uint32(len(s) + 1))
	w.raw = append(w.raw, []byte(s)...)
	w.raw = append(w.raw, 0)
}
func (w *cdrFixture) header() { w.u32(5); w.u32(20); w.str("laser") }
func laserFixture() []byte {
	w := newCDRFixture(false)
	w.header()
	for _, f := range []float32{0, math.Pi, math.Pi / 2, 0, .1, .1, 10} {
		w.f32(f)
	}
	w.u32(4)
	for _, f := range []float32{1, 2, float32(math.Inf(1)), 20} {
		w.f32(f)
	}
	w.u32(0)
	return w.raw
}
func TestVisualLaserDecodeAndMalformedCDR(t *testing.T) {
	decoded, frame, err := decodeVisualROS("sensor_msgs/msg/LaserScan", laserFixture())
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Points) != 2 || decoded.Points[0].X != 1 || math.Abs(float64(decoded.Points[1].Y)-2) > 1e-5 || !strings.Contains(frame, "laser") {
		t.Fatalf("bad scan: %+v %s", decoded, frame)
	}
	for i := 0; i < len(laserFixture())-4; i++ {
		if _, _, err := decodeVisualROS("sensor_msgs/msg/LaserScan", laserFixture()[:i]); err == nil {
			t.Fatalf("accepted truncated scan at %d", i)
		}
	}
}
func TestVisualPointCloudEndiannessPaddingAndBounds(t *testing.T) {
	for _, big := range []bool{false, true} {
		w := newCDRFixture(big)
		w.header()
		w.u32(2)
		w.u32(1)
		w.u32(3)
		for i, axis := range []string{"x", "y", "z"} {
			w.str(axis)
			w.u32(uint32(i * 4))
			w.raw = append(w.raw, 7)
			w.u32(1)
		}
		if big {
			w.raw = append(w.raw, 1)
		} else {
			w.raw = append(w.raw, 0)
		}
		w.u32(12)
		w.u32(16)
		w.u32(32)
		for _, value := range []float32{1, 2, 3, 999, 4, 5, 6, 999} {
			var b [4]byte
			w.order.PutUint32(b[:], math.Float32bits(value))
			w.raw = append(w.raw, b[:]...)
		}
		w.raw = append(w.raw, 1)
		got, _, err := decodeVisualROS("sensor_msgs/msg/PointCloud2", w.raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Points) != 2 || got.Points[1] != (VisualPoint{4, 5, 6}) {
			t.Fatalf("row padding or endianness lost: %+v", got)
		}
		if _, _, err := decodeVisualROS("sensor_msgs/msg/PointCloud2", w.raw[:len(w.raw)-10]); err == nil {
			t.Fatal("accepted truncated point data")
		}
	}
}
func TestVisualROSClockMappingRefusesMissingDistantAndResetClocks(t *testing.T) {
	clocks := []visualROSClock{{Episode: -500, Sample: data.ClockSample{TargetNanos: 10_000_000_000}}, {Episode: 999_999_500, Sample: data.ClockSample{TargetNanos: 11_000_000_000}}}
	got, err := rosEpisodeTime(10_000_000_100, clocks)
	if err != nil || got != -400 {
		t.Fatalf("wrong alignment %d %v", got, err)
	}
	if _, err := rosEpisodeTime(0, clocks); err == nil {
		t.Fatal("accepted distant clock")
	}
	if _, err := rosEpisodeTime(1, nil); err == nil {
		t.Fatal("fabricated clock")
	}
	var raw bytes.Buffer
	for _, c := range clocks {
		c.Kind = "host_realtime_sandwich"
		if err := json.NewEncoder(&raw).Encode(c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := readROSClocks(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	reset := visualROSClock{Kind: "host_realtime_sandwich", Episode: 2e9, Sample: data.ClockSample{TargetNanos: 1e9}}
	_ = json.NewEncoder(&raw).Encode(reset)
	if _, err := readROSClocks(raw.Bytes()); err == nil {
		t.Fatal("accepted realtime reset")
	}
}
func TestVisualROSBagsReadSQLiteAndCompressedMCAP(t *testing.T) {
	for _, format := range []string{".db3", ".mcap"} {
		t.Run(format, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "bag"+format)
			payload := laserFixture()
			if format == ".db3" {
				db, err := sql.Open("sqlite3", name)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				for _, q := range []string{`CREATE TABLE topics(id INTEGER PRIMARY KEY,name TEXT,type TEXT,serialization_format TEXT)`, `CREATE TABLE messages(id INTEGER PRIMARY KEY,topic_id INTEGER,timestamp INTEGER,data BLOB)`, `INSERT INTO topics VALUES(1,'/scan','sensor_msgs/msg/LaserScan','cdr')`} {
					if _, err := db.Exec(q); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := db.Exec(`INSERT INTO messages VALUES(1,1,10000000100,?)`, payload); err != nil {
					t.Fatal(err)
				}
			} else {
				f, err := os.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				w, err := mcap.NewWriter(f, &mcap.WriterOptions{Chunked: true, Compression: mcap.CompressionZSTD})
				if err != nil {
					t.Fatal(err)
				}
				for _, err := range []error{w.WriteHeader(&mcap.Header{Profile: "ros2"}), w.WriteSchema(&mcap.Schema{ID: 1, Name: "sensor_msgs/msg/LaserScan", Encoding: "ros2msg", Data: []byte("fixture")}), w.WriteChannel(&mcap.Channel{ID: 1, SchemaID: 1, Topic: "/scan", MessageEncoding: "cdr"}), w.WriteMessage(&mcap.Message{ChannelID: 1, LogTime: 10_000_000_100, Data: payload}), w.Close(), f.Close()} {
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			v := &VisualSnapshot{}
			budget := 100
			err := v.readROSBag(context.Background(), name, format, "ros2:default:domain-0", []visualROSClock{{Episode: -500, Sample: data.ClockSample{TargetNanos: 10_000_000_000}}}, &budget)
			if err != nil {
				t.Fatal(err)
			}
			if len(v.Samples) != 1 || v.Samples[0].Time != -400 || len(v.Samples[0].Points) != 2 || v.Tracks[0].Kind != "lidar" {
				t.Fatalf("wrong bag preview: %+v", v)
			}
		})
	}
}
func audioFixture() []byte {
	raw := make([]byte, 44+4800)
	copy(raw, "RIFF")
	binary.LittleEndian.PutUint32(raw[4:], uint32(len(raw)-8))
	copy(raw[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(raw[16:], 16)
	binary.LittleEndian.PutUint16(raw[20:], 1)
	binary.LittleEndian.PutUint16(raw[22:], 1)
	binary.LittleEndian.PutUint32(raw[24:], 48000)
	binary.LittleEndian.PutUint32(raw[28:], 96000)
	binary.LittleEndian.PutUint16(raw[32:], 2)
	binary.LittleEndian.PutUint16(raw[34:], 16)
	copy(raw[36:], "data")
	binary.LittleEndian.PutUint32(raw[40:], 4800)
	binary.LittleEndian.PutUint16(raw[44:], 16384)
	return raw
}
func addVisualFile(c *visualFixtureClient, name, source string, raw []byte) {
	sum := sha256.Sum256(raw)
	c.files[name] = raw
	c.manifest.Files = append(c.manifest.Files, data.File{Path: name, SourceID: source, Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:])})
}
func TestVisualAudioUsesVerifiedSegmentTimeAndRejectsDurationMismatch(t *testing.T) {
	c := &visualFixtureClient{manifest: data.Manifest{ID: "episode", State: "complete", CanonicalClock: "CLOCK_BOOTTIME"}, files: map[string][]byte{}}
	addVisualFile(c, "audio/mic/a.wav", "audio:mic", audioFixture())
	addVisualFile(c, "audio/mic/index.jsonl", "audio:mic", []byte(`{"canonical_episode_nanos":2000000000,"canonical_uncertainty_nanos":50000000,"segment":"audio/mic/a.wav","byte_size":4800,"sample_rate":48000,"channels":1,"duration_nanos":50000000}`))
	v, err := readVisualEpisode(context.Background(), c, "episode")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, a := range v.Audio {
			_ = os.Remove(a.Path)
		}
	}()
	if len(v.Audio) != 1 || v.Audio[0].Start != 2e9 || v.Audio[0].End != 2050000000 || len(v.Samples) != 1 || v.Samples[0].Value != .5 {
		t.Fatalf("bad audio %+v", v)
	}
	start := int64(0)
	if _, err := visualWAVPeaks(v.Audio[0].Path, visualAudioIndex{Start: &start, Bytes: 4800, Rate: 48000, Channels: 1, Duration: 99}); err == nil {
		t.Fatal("accepted invented duration")
	}
}
func TestVisualExportPreservesFilesAndRefusesUnsafeOrUnverifiedArchives(t *testing.T) {
	c := &visualFixtureClient{manifest: data.Manifest{ID: "episode", State: "complete"}, files: map[string][]byte{}}
	addVisualFile(c, "ros2/domain/metadata.yaml", "ros2:default:domain-0", []byte("original metadata"))
	addVisualFile(c, "audio/mic/a.wav", "audio:mic", audioFixture())
	filename, err := exportVisualEpisode(context.Background(), c, "episode")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(filename)
	z, err := zip.OpenReader(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	found := map[string][]byte{}
	for _, f := range z.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		found[f.Name], err = io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, raw := range c.files {
		if !bytes.Equal(found[name], raw) {
			t.Fatalf("export altered %s", name)
		}
	}
	if len(found["manifest.json"]) == 0 {
		t.Fatal("missing manifest")
	}
	original := c.manifest.Files[0]
	for _, bad := range []string{"../escape", "/absolute", "a/../../bad", "a\\bad", "manifest.json", "."} {
		c.manifest.Files[0].Path = bad
		if filename, err := exportVisualEpisode(context.Background(), c, "episode"); err == nil {
			os.Remove(filename)
			t.Fatalf("accepted unsafe path %q", bad)
		}
	}
	c.manifest.Files[0] = original
	c.mutate = func(chunk *pb.DataDownloadChunk) { chunk.Data = bytes.Repeat([]byte{'x'}, len(chunk.Data)) }
	if filename, err := exportVisualEpisode(context.Background(), c, "episode"); err == nil {
		os.Remove(filename)
		t.Fatal("export accepted checksum mismatch")
	}
	c.mutate = nil
	c.manifest.State = "recording"
	if _, err := exportVisualEpisode(context.Background(), c, "episode"); err == nil {
		t.Fatal("exported unsealed recording")
	}
}
