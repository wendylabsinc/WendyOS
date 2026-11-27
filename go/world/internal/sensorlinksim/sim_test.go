package sensorlinksim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/sensorlink"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func fixture(t *testing.T, frames int) (string, Capture) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "blobs"), 0700); err != nil {
		t.Fatal(err)
	}
	pixels := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			pixels.Set(x, y, color.RGBA{R: uint8(x * 7), G: uint8(y * 5), B: 12, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, pixels); err != nil {
		t.Fatal(err)
	}
	media := encoded.Bytes()
	digest := hash(media)
	if err := os.WriteFile(filepath.Join(dir, "blobs", digest), media, 0600); err != nil {
		t.Fatal(err)
	}
	var lines bytes.Buffer
	for i := 1; i <= frames; i++ {
		s := sensing.Sample{SchemaVersion: sensing.Schema, ID: "capture/camera-a/" + string(rune('0'+i)), SiteID: "synthetic-office", Frame: world.FrameRef{ID: "floor", Revision: "v1"}, SensorID: "camera-a", Kind: "camera", Sequence: uint64(i), SampleNS: 250000000 + int64(i-1)*10000000, RunID: "capture", ScenarioVersion: "scene-v1", Seed: 7, PackageSHA256: strings.Repeat("a", 64), SensorModel: "synthetic-camera", PositionM: []float64{0, 0, 0}, UncertaintyM: .01, CalibrationRevision: "cal-v1", Camera: &sensing.Camera{Width: 32, Height: 24, FX: 10, FY: 10, CX: 16, CY: 12, CameraToSite: []float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}, Evidence: &sensing.EvidenceRef{SHA256: digest, ContentType: "image/png", Size: int64(len(media))}}
		if err := json.NewEncoder(&lines).Encode(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "samples.jsonl"), lines.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	report := runReport{SchemaVersion: "wendy.sensor.run.v1", Synthetic: true, PackageSHA256: strings.Repeat("a", 64), Samples: frames, BlobBytes: int64(len(media)), AssetLicense: "synthetic test", Fidelity: map[string]string{"camera": "generated fixture"}}
	report.Configuration.Frames = frames
	report.Configuration.IntervalMS = 10
	report.Configuration.Seed = 7
	report.Configuration.RunID = "capture"
	report.Configuration.Omit = []string{"camera-b", "gas-1", "audio-1"}
	raw, _ := json.Marshal(report)
	if err := os.WriteFile(filepath.Join(dir, "run.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	capture, err := Load(dir, "camera-a", "capture", 90)
	if err != nil {
		t.Fatal(err)
	}
	return dir, capture
}

func TestLoadedCaptureOwnsVerifiedJPEG(t *testing.T) {
	dir, capture := fixture(t, 3)
	if len(capture.Frames) != 3 || capture.Width != 32 || capture.Height != 24 {
		t.Fatal(capture)
	}
	before := append([]byte(nil), capture.Frames[0].JPEG...)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := capture.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(capture.Frames[0].JPEG)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, capture.Frames[0].JPEG) {
		t.Fatal("verified bytes changed")
	}
}

func TestCaptureRejectsCorruptionAndFalseLineage(t *testing.T) {
	for _, test := range []string{"media", "digest", "dimensions", "synthetic", "run", "sequence", "calibration", "missing", "symlink", "report-count", "oversize"} {
		t.Run(test, func(t *testing.T) {
			dir, capture := fixture(t, 2)
			switch test {
			case "media":
				os.WriteFile(filepath.Join(dir, "blobs", capture.Frames[0].Sample.Evidence.SHA256), []byte("bad"), 0600)
			case "missing":
				os.Remove(filepath.Join(dir, "blobs", capture.Frames[0].Sample.Evidence.SHA256))
			case "symlink":
				p := filepath.Join(dir, "blobs", capture.Frames[0].Sample.Evidence.SHA256)
				b, _ := os.ReadFile(p)
				os.Remove(p)
				os.WriteFile(filepath.Join(dir, "alternate"), b, 0600)
				if err := os.Symlink("../alternate", p); err != nil {
					t.Fatal(err)
				}
			case "synthetic", "run", "report-count":
				b, _ := os.ReadFile(filepath.Join(dir, "run.json"))
				var report runReport
				json.Unmarshal(b, &report)
				if test == "synthetic" {
					report.Synthetic = false
				}
				if test == "run" {
					report.Configuration.RunID = "different"
				}
				if test == "report-count" {
					report.Samples++
				}
				b, _ = json.Marshal(report)
				os.WriteFile(filepath.Join(dir, "run.json"), b, 0600)
			case "oversize":
				os.WriteFile(filepath.Join(dir, "run.json"), bytes.Repeat([]byte(" "), 65537), 0600)
			default:
				b, _ := os.ReadFile(filepath.Join(dir, "samples.jsonl"))
				lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
				var s sensing.Sample
				json.Unmarshal(lines[1], &s)
				switch test {
				case "digest":
					s.Evidence.SHA256 = strings.Repeat("b", 64)
				case "dimensions":
					s.Camera.Width++
				case "sequence":
					s.Sequence = 3
					s.ID = "capture/camera-a/3"
				case "calibration":
					s.CalibrationRevision = "new"
				}
				lines[1], _ = json.Marshal(s)
				os.WriteFile(filepath.Join(dir, "samples.jsonl"), bytes.Join(lines, []byte("\n")), 0600)
			}
			if _, err := Load(dir, "camera-a", "capture", 90); err == nil {
				t.Fatal("accepted invalid capture")
			}
		})
	}
}

func TestWireChunksRetainJPEGAndTimestamp(t *testing.T) {
	_, capture := fixture(t, 3)
	server, client := net.Pipe()
	defer client.Close()
	anchor := time.Now().Add(-300 * time.Millisecond)
	cfg := Config{ChannelID: 1, Anchor: anchor, ChunkBytes: 64}
	result := make(chan Stats, 1)
	failure := make(chan error, 1)
	go func() {
		stats, err := ServeConn(context.Background(), server, capture, cfg)
		result <- stats
		failure <- err
	}()
	if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	manifest, err := sensorlink.ReadMessage(client)
	if err != nil {
		t.Fatal(err)
	}
	sensors := manifest.GetManifest().Sensors
	if len(sensors) != 1 || sensors[0].ChannelId != 1 || sensors[0].GetVideo().Codec != pb.VideoFormat_MJPEG || sensors[0].GetVideo().Width != 32 {
		t.Fatal(sensors)
	}
	err = sensorlink.WriteMessage(client, &pb.Envelope{Msg: &pb.Envelope_Subscribe{Subscribe: &pb.Subscribe{ChannelId: []uint32{1}}}})
	if err != nil {
		t.Fatal(err)
	}
	for i, frame := range capture.Frames {
		var jpegBytes []byte
		chunk := uint32(0)
		for {
			env, err := sensorlink.ReadMessage(client)
			if err != nil {
				t.Fatal(err)
			}
			data := env.GetData()
			if data.ChannelId != 1 || data.FrameSeq != uint32(i) || data.ChunkSeq != chunk || data.TsUs != uint64(anchor.UnixMicro()+frame.Sample.SampleNS/1000) {
				t.Fatal(data)
			}
			if chunk == 0 && data.Flags&sensorlink.FlagKeyframe == 0 {
				t.Fatal("first chunk lacks keyframe")
			}
			jpegBytes = append(jpegBytes, data.Payload...)
			chunk++
			if data.Flags&sensorlink.FlagLastChunk != 0 {
				break
			}
		}
		if !bytes.Equal(jpegBytes, frame.JPEG) {
			t.Fatal("wire changed encoded JPEG")
		}
	}
	if _, err = sensorlink.ReadMessage(client); err != io.EOF {
		t.Fatal(err)
	}
	if err = <-failure; err != nil {
		t.Fatal(err)
	}
	stats := <-result
	if !stats.Subscribed || stats.FramesSent != 3 || stats.ChunksSent <= 3 || stats.ChunksOmitted != 0 {
		t.Fatal(stats)
	}
	for i, report := range stats.Frames {
		if report.JPEGHash != hash(capture.Frames[i].JPEG) || report.Bytes != len(capture.Frames[i].JPEG) {
			t.Fatal(report)
		}
	}
}

func TestDroppedMiddleChunkIsReportedOnWire(t *testing.T) {
	_, capture := fixture(t, 3)
	server, client := net.Pipe()
	defer client.Close()
	cfg := Config{ChannelID: 1, Anchor: time.Now().Add(-time.Second), ChunkBytes: 64, Drop: &DropChunk{FrameSequence: 1, ChunkSequence: 1}}
	done := make(chan Stats, 1)
	failure := make(chan error, 1)
	go func() {
		stats, err := ServeConn(context.Background(), server, capture, cfg)
		done <- stats
		failure <- err
	}()
	client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := sensorlink.ReadMessage(client); err != nil {
		t.Fatal(err)
	}
	sensorlink.WriteMessage(client, &pb.Envelope{Msg: &pb.Envelope_Subscribe{Subscribe: &pb.Subscribe{ChannelId: []uint32{1}}}})
	chunks := map[uint32][]uint32{}
	for {
		env, err := sensorlink.ReadMessage(client)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		d := env.GetData()
		chunks[d.FrameSeq] = append(chunks[d.FrameSeq], d.ChunkSeq)
	}
	if err := <-failure; err != nil {
		t.Fatal(err)
	}
	stats := <-done
	if stats.ChunksOmitted != 1 || stats.Frames[1].OmittedChunks != 1 {
		t.Fatal(stats)
	}
	for _, number := range chunks[1] {
		if number == 1 {
			t.Fatal("omitted chunk was sent")
		}
	}
	if !reflect.DeepEqual(chunks[0], chunks[2]) {
		t.Fatal("unaffected frames changed")
	}
}

func TestSubscriptionAndCancellationFailClosed(t *testing.T) {
	_, capture := fixture(t, 1)
	for _, channels := range [][]uint32{nil, {2}, {1, 1}, {1, 2}} {
		server, client := net.Pipe()
		done := make(chan error, 1)
		go func() {
			_, err := ServeConn(context.Background(), server, capture, Config{ChannelID: 1, Anchor: time.Now(), ChunkBytes: 64})
			done <- err
		}()
		client.SetDeadline(time.Now().Add(time.Second))
		sensorlink.ReadMessage(client)
		sensorlink.WriteMessage(client, &pb.Envelope{Msg: &pb.Envelope_Subscribe{Subscribe: &pb.Subscribe{ChannelId: channels}}})
		if err := <-done; err == nil {
			t.Fatal("bad subscription accepted")
		}
		client.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		_, err := ServeConn(ctx, server, capture, Config{ChannelID: 1, Anchor: time.Now().Add(30 * time.Second), ChunkBytes: 64})
		done <- err
	}()
	client.SetDeadline(time.Now().Add(time.Second))
	sensorlink.ReadMessage(client)
	sensorlink.WriteMessage(client, &pb.Envelope{Msg: &pb.Envelope_Subscribe{Subscribe: &pb.Subscribe{ChannelId: []uint32{1}}}})
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt schedule")
	}
}

func TestAcceptAndHandshakeCancel(t *testing.T) {
	_, capture := fixture(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Serve(ctx, listener, capture, Config{ChannelID: 1, Anchor: time.Now(), ChunkBytes: 64})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("accept stuck")
	}
	server, client := net.Pipe()
	defer client.Close()
	ctx, cancel = context.WithCancel(context.Background())
	go func() {
		_, err := ServeConn(ctx, server, capture, Config{ChannelID: 1, Anchor: time.Now(), ChunkBytes: 64})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("handshake stuck")
	}
}

func TestWireClientDropsIncompleteFrameAndRecovers(t *testing.T) {
	_, capture := fixture(t, 3)
	server, client := tcpPair(t)
	cfg := Config{ChannelID: 1, Anchor: time.Now().Add(-200 * time.Millisecond), ChunkBytes: 64, Drop: &DropChunk{FrameSequence: 1, ChunkSequence: 1}}
	done := make(chan error, 1)
	go func() { _, err := ServeConn(context.Background(), server, capture, cfg); done <- err }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := sensorlink.Connect(ctx, client, []uint32{1}, sensorlink.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for _, index := range []int{0, 2} {
		frame, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Seq != uint32(index) || !bytes.Equal(frame.Payload, capture.Frames[index].JPEG) {
			t.Fatal("partial or altered frame escaped", frame.Seq)
		}
	}
	if _, err = stream.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTimestampFaultChangesWireTimeOnly(t *testing.T) {
	_, capture := fixture(t, 1)
	server, client := net.Pipe()
	defer client.Close()
	anchor := time.Now().Add(-time.Second)
	done := make(chan error, 1)
	go func() {
		_, err := ServeConn(context.Background(), server, capture, Config{ChannelID: 1, Anchor: anchor, ChunkBytes: 65536, TimestampOffsetUS: 2000000})
		done <- err
	}()
	client.SetDeadline(time.Now().Add(time.Second))
	sensorlink.ReadMessage(client)
	sensorlink.WriteMessage(client, &pb.Envelope{Msg: &pb.Envelope_Subscribe{Subscribe: &pb.Subscribe{ChannelId: []uint32{1}}}})
	env, err := sensorlink.ReadMessage(client)
	if err != nil {
		t.Fatal(err)
	}
	data := env.GetData()
	if data.TsUs != uint64(anchor.UnixMicro()+capture.Frames[0].Sample.SampleNS/1000+2000000) || !bytes.Equal(data.Payload, capture.Frames[0].JPEG) {
		t.Fatal("timestamp fault changed unexpected data")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPingsKeepScheduledSourceAlive(t *testing.T) {
	_, capture := fixture(t, 1)
	server, client := tcpPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	anchor := time.Now().Add(150 * time.Millisecond)
	type result struct {
		stats Stats
		err   error
	}
	done := make(chan result, 1)
	go func() {
		stats, err := ServeConn(ctx, server, capture, Config{ChannelID: 1, Anchor: anchor, ChunkBytes: 512, PingInterval: 20 * time.Millisecond})
		done <- result{stats, err}
	}()
	stream, err := sensorlink.Connect(ctx, client, []uint32{1}, sensorlink.Options{IdleTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	frame, err := stream.Next(ctx)
	if err != nil {
		t.Fatal("pings did not preserve the idle session", err)
	}
	if frame.Seq != 0 || frame.TsUs != uint64(anchor.UnixMicro()+capture.Frames[0].Sample.SampleNS/1000) || !bytes.Equal(frame.Payload, capture.Frames[0].JPEG) {
		t.Fatal("keepalive changed capture timestamp or media")
	}
	if _, err = stream.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatal("ping created an observation or prevented EOF", err)
	}
	got := <-done
	if got.err != nil || got.stats.PingsSent < 2 || got.stats.FramesSent != 1 {
		t.Fatal("unexpected source report", got.stats, got.err)
	}
}

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	return server, client
}

func TestWaitingPingWritesAreBounded(t *testing.T) {
	_, capture := fixture(t, 1)
	for _, cancelWrite := range []bool{false, true} {
		t.Run(fmt.Sprint("cancel=", cancelWrite), func(t *testing.T) {
			server, client := net.Pipe()
			defer client.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := ServeConn(ctx, server, capture, Config{ChannelID: 1, Anchor: time.Now().Add(time.Second), ChunkBytes: 512, PingInterval: 10 * time.Millisecond, WriteTimeout: 50 * time.Millisecond})
				done <- err
			}()
			client.SetDeadline(time.Now().Add(time.Second))
			if _, err := sensorlink.ReadMessage(client); err != nil {
				t.Fatal(err)
			}
			if err := sensorlink.WriteMessage(client, &pb.Envelope{Msg: &pb.Envelope_Subscribe{Subscribe: &pb.Subscribe{ChannelId: []uint32{1}}}}); err != nil {
				t.Fatal(err)
			}
			// Read only the ping header, leaving its body write blocked.
			var header [4]byte
			if _, err := io.ReadFull(client, header[:]); err != nil {
				t.Fatal(err)
			}
			if cancelWrite {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("blocked ping succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("blocked ping ignored cancellation or write deadline")
			}
		})
	}
}

func TestMissingZeroValuedReportFieldsRejected(t *testing.T) {
	for _, field := range []string{"seed", "alarm", "omit"} {
		t.Run(field, func(t *testing.T) {
			dir, _ := fixture(t, 1)
			file := filepath.Join(dir, "run.json")
			b, _ := os.ReadFile(file)
			var report map[string]any
			json.Unmarshal(b, &report)
			delete(report["configuration"].(map[string]any), field)
			b, _ = json.Marshal(report)
			os.WriteFile(file, b, 0600)
			if _, err := Load(dir, "camera-a", "capture", 90); err == nil {
				t.Fatal("missing field defaulted")
			}
		})
	}
}
