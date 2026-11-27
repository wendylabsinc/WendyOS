package livesensorlink

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/livecapture"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/sensorlink"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

var testToken = strings.Repeat("r", 32)

func testCommission() Commission {
	return Commission{RunID: "run", BootID: "scene-boot", PackageSHA256: strings.Repeat("a", 64), Site: "office", Frame: world.FrameRef{ID: "floor", Revision: "survey-1"}, SensorID: "camera-b", AnchorUnixNS: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()}
}
func testSourceConfig(t *testing.T) SourceConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "read-token")
	if e := os.WriteFile(path, []byte(testToken), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := (SourceConfig{Commission: testCommission(), Bounds: Bounds{DurationMS: 8000, MaxFrames: 2}, IsolatedSimulationNetwork: true, SceneURL: "http://127.0.0.1:1", SceneReadTokenFile: path, MetadataReadTokenFile: path, TCPListen: "127.0.0.1:50060", MetadataListen: "127.0.0.1:50061", ChannelID: 4, ChunkBytes: 64, JPEGQuality: 85, PollIntervalMS: 250}).normalize()
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func testSnapshot(t *testing.T, seq uint64, boot string) livecapture.Snapshot {
	t.Helper()
	c := testCommission()
	now := time.Now().UnixMilli()
	sim := int64(seq) * int64(time.Second)
	position := [3]float64{-2 + float64(seq)/100, .2, 0}
	s := livecapture.Snapshot{SchemaVersion: livecapture.Schema, RunID: c.RunID, BootID: boot, Sequence: seq, SampleUnixMS: now, SimulationTimeNS: sim, Observations: []sensing.Sample{}, Media: []livecapture.Media{}}
	s.State = livecapture.State{SchemaVersion: "wendy.sim.actuator.v0", Synthetic: true, BootID: boot, Session: "fixture", SiteID: c.Site, Frame: c.Frame, Position: position, SampleUnixMS: now, SimulationTimeS: float64(sim) / 1e9, Sensors: map[string]bool{"gas": true, "local-obstacles": true}, ObstacleClear: true, Energy: 1, WatchdogStopped: true, Provenance: livecapture.Provenance{Mode: "simulated", PackageSHA256: c.PackageSHA256, RobotModel: "synthetic-test-puck", GasModel: "analytic-test-gas"}}
	for _, id := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
		x := sensing.Sample{SchemaVersion: sensing.Schema, ID: fmt.Sprintf("run/%s/%d", id, seq), SiteID: c.Site, Frame: c.Frame, SensorID: id, Sequence: seq, SampleNS: sim, RunID: c.RunID, ScenarioVersion: "mujoco4-fixture", Seed: 7, PackageSHA256: c.PackageSHA256, SensorModel: "test-only", PositionM: append([]float64(nil), position[:]...), UncertaintyM: .02, CalibrationRevision: fmt.Sprintf("cal-%d", seq)}
		var data []byte
		mime := ""
		switch id {
		case "gas-1":
			x.Kind = "gas"
			x.Gas = &sensing.Gas{ValuePPM: 70, UncertaintyPPM: 2, Species: "test-gas"}
		case "audio-1":
			x.Kind = "audio"
			x.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(250 * time.Millisecond)}
			data = make([]byte, 4044)
			copy(data, "RIFF")
			binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
			copy(data[8:], "WAVEfmt ")
			binary.LittleEndian.PutUint32(data[16:], 16)
			binary.LittleEndian.PutUint16(data[20:], 1)
			binary.LittleEndian.PutUint16(data[22:], 1)
			binary.LittleEndian.PutUint32(data[24:], 8000)
			binary.LittleEndian.PutUint32(data[28:], 16000)
			binary.LittleEndian.PutUint16(data[32:], 2)
			binary.LittleEndian.PutUint16(data[34:], 16)
			copy(data[36:], "data")
			binary.LittleEndian.PutUint32(data[40:], 4000)
			mime = "audio/wav"
		default:
			x.Kind = "camera"
			if id == "camera-b" {
				x.PositionM[1] += 1.8
				x.PositionM[2] += 2
			}
			x.Camera = &sensing.Camera{Width: 16, Height: 12, FX: 16 + float64(seq)/100, FY: 16, CX: 8, CY: 6, CameraToSite: []float64{1, 0, 0, x.PositionM[0], 0, 1, 0, x.PositionM[1], 0, 0, 1, x.PositionM[2], 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}
			im := image.NewNRGBA(image.Rect(0, 0, 16, 12))
			state := uint32(seq) + uint32(len(id))
			if id == "camera-b" {
				state += 99
			}
			for i := range im.Pix {
				state = state*1664525 + 1013904223
				im.Pix[i] = byte(state >> 24)
			}
			for i := 3; i < len(im.Pix); i += 4 {
				im.Pix[i] = 255
			}
			var encoded bytes.Buffer
			if e := png.Encode(&encoded, im); e != nil {
				t.Fatal(e)
			}
			data = encoded.Bytes()
			mime = "image/png"
		}
		if len(data) > 0 {
			x.Evidence = &sensing.EvidenceRef{SHA256: digest(data), Size: int64(len(data)), ContentType: mime}
			s.Media = append(s.Media, livecapture.Media{SHA256: x.Evidence.SHA256, Data: data})
		}
		s.Observations = append(s.Observations, x)
	}
	return s
}

func testFrame(t *testing.T, cfg SourceConfig, seq uint32) preparedFrame {
	t.Helper()
	f, e := prepare(testSnapshot(t, uint64(seq)+16, cfg.Commission.BootID), cfg, seq)
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func received(f preparedFrame) sensorlink.Frame {
	m := f.metadata
	return sensorlink.Frame{ChannelID: m.ChannelID, Seq: m.FrameSequence, TsUs: m.TimestampUS, Flags: m.Flags, Payload: bytes.Clone(f.jpeg)}
}

func TestMetadataCacheIsReadOnlyExactAndBounded(t *testing.T) {
	cfg := testSourceConfig(t)
	cache := newCache(testToken)
	first := testFrame(t, cfg, 1)
	if e := cache.put(first.metadata); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(cache)
	defer server.Close()
	request := func(method, path, token string) int {
		t.Helper()
		r, e := http.NewRequest(method, server.URL+path, nil)
		if e != nil {
			t.Fatal(e)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		response, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	if request("GET", metadataPath(first.metadata), testToken) != 200 || request("GET", metadataPath(first.metadata), "wrong") != 401 || request("POST", metadataPath(first.metadata), testToken) != 405 || request("GET", "/v1/actuator", testToken) != 404 {
		t.Fatal("metadata endpoint was not exact and read-only")
	}
	changed := first.metadata
	capture, e := sensorrecord.Decode(changed.CaptureJSON)
	if e != nil {
		t.Fatal(e)
	}
	capture.SceneBootID = "another-boot"
	changed.CaptureJSON, e = sensorrecord.Encode(capture)
	if e != nil {
		t.Fatal(e)
	}
	if e = cache.put(changed); !errors.Is(e, ErrMetadata) {
		t.Fatal("same wire key accepted different capture bytes")
	}
	for i := uint32(2); i <= 17; i++ {
		next := first.metadata
		next.FrameSequence = i
		if e := cache.put(next); e != nil {
			t.Fatal(e)
		}
	}
	if len(cache.entries) != 16 || request("GET", metadataPath(first.metadata), testToken) != 404 {
		t.Fatal("metadata cache exceeded its frame bound or retained evicted frame")
	}
	cache.mu.Lock()
	cache.now = func() time.Time { return time.Unix(0, capture.CapturedUnixNS).Add(MaxCaptureAge + time.Nanosecond) }
	cache.mu.Unlock()
	last := first.metadata
	last.FrameSequence = 17
	if request("GET", metadataPath(last), testToken) != 404 {
		t.Fatal("old original capture metadata was renewed")
	}
}

func TestMetadataClientRejectsMismatchesAndLatches(t *testing.T) {
	for _, name := range []string{"sequence", "timestamp", "flags", "digest", "size", "boot", "run", "package", "anchor", "frame", "stale", "redirect", "oversize"} {
		t.Run(name, func(t *testing.T) {
			cfg := testSourceConfig(t)
			f := testFrame(t, cfg, 1)
			meta := f.metadata
			switch name {
			case "sequence":
				meta.FrameSequence++
			case "timestamp":
				meta.TimestampUS++
			case "flags":
				meta.Flags = 0
			case "digest":
				meta.JPEGSHA256 = strings.Repeat("b", 64)
			case "size":
				meta.JPEGSize++
			}
			if name == "boot" || name == "run" || name == "package" || name == "anchor" || name == "frame" {
				c, _ := sensorrecord.Decode(meta.CaptureJSON)
				s, _ := c.Sample()
				switch name {
				case "boot":
					c.SceneBootID = "restarted"
				case "run":
					s.RunID = "other"
					s.ID = fmt.Sprintf("other/%s/%d", s.SensorID, s.Sequence)
				case "package":
					s.PackageSHA256 = strings.Repeat("b", 64)
				case "anchor":
					c.SimulationAnchorUnixNS++
				case "frame":
					s.Frame.Revision = "other"
				}
				c.SampleJSON, _ = json.Marshal(s)
				var e error
				meta.CaptureJSON, e = sensorrecord.Encode(c)
				if e != nil {
					t.Fatal(e)
				}
			}
			var redirected atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
			defer other.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if name == "redirect" {
					http.Redirect(w, r, other.URL, 302)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if name == "oversize" {
					_, _ = w.Write(bytes.Repeat([]byte("x"), MaxMetadataBytes+1))
					return
				}
				_ = json.NewEncoder(w).Encode(meta)
			}))
			defer server.Close()
			bridge := BridgeConfig{Commission: cfg.Commission, Bounds: cfg.Bounds, MetadataURL: server.URL}
			client := newMetadataClient(bridge, testToken)
			defer client.http.CloseIdleConnections()
			if name == "stale" {
				c, _ := sensorrecord.Decode(meta.CaptureJSON)
				client.now = func() time.Time { return time.Unix(0, c.CapturedUnixNS).Add(MaxCaptureAge + time.Nanosecond) }
			}
			if _, e := client.fetch(context.Background(), received(f)); !errors.Is(e, ErrMetadata) {
				t.Fatalf("inconsistent metadata accepted: %v", e)
			}
			meta = f.metadata
			client.now = time.Now
			if _, e := client.fetch(context.Background(), received(f)); !errors.Is(e, ErrMetadata) {
				t.Fatal("faulted metadata session silently resumed")
			}
			if redirected.Load() != 0 {
				t.Fatal("read token followed redirect")
			}
		})
	}
}

func TestMetadataRejectsWireReplayAndWrap(t *testing.T) {
	cfg := testSourceConfig(t)
	cache := newCache(testToken)
	f := testFrame(t, cfg, 1)
	if e := cache.put(f.metadata); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(cache)
	defer server.Close()
	client := newMetadataClient(BridgeConfig{Commission: cfg.Commission, Bounds: cfg.Bounds, MetadataURL: server.URL}, testToken)
	defer client.http.CloseIdleConnections()
	if _, e := client.fetch(context.Background(), received(f)); e != nil {
		t.Fatal(e)
	}
	if _, e := client.fetch(context.Background(), received(f)); !errors.Is(e, ErrMetadata) {
		t.Fatal("wire frame replay accepted")
	}
	wrapped := newMetadataClient(BridgeConfig{Commission: cfg.Commission, Bounds: cfg.Bounds, MetadataURL: server.URL}, testToken)
	defer wrapped.http.CloseIdleConnections()
	wrapped.lastFrame = ^uint32(0)
	if _, e := wrapped.fetch(context.Background(), received(f)); !errors.Is(e, ErrMetadata) {
		t.Fatal("wire sequence wrap accepted")
	}
}

type sourceResult struct {
	result Result
	err    error
}

func startSource(t *testing.T, cfg SourceConfig, sceneURL string) (string, string, <-chan sourceResult) {
	t.Helper()
	client, e := livecapture.New(livecapture.Config{URL: sceneURL, Token: testToken, RunID: cfg.Commission.RunID, Site: cfg.Commission.Site, Frame: cfg.Commission.Frame})
	if e != nil {
		t.Fatal(e)
	}
	cache := newCache(testToken)
	server := httptest.NewServer(cache)
	t.Cleanup(server.Close)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = listener.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	t.Cleanup(cancel)
	done := make(chan sourceResult, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			done <- sourceResult{err: e}
			return
		}
		result, e := serveConn(ctx, conn, client, cache, cfg)
		done <- sourceResult{result, e}
	}()
	return listener.Addr().String(), server.URL, done
}

type rpcClock struct{ now time.Time }

func (c rpcClock) Now() (trustedtime.Interval, error) {
	return trustedtime.Interval{Earliest: c.now, Latest: c.now.Add(time.Millisecond), BootID: "fixture", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-only"}, nil
}

type rpcServer struct {
	appv1.UnimplementedWorldAppServiceServer
	mu              sync.Mutex
	publisher       *serviceorigin.Publisher
	uploads         map[string][]byte
	requests        []*appv1.PublishRequest
	until           int64
	publicationGate <-chan struct{}
}

func (s *rpcServer) PutEvidence(_ context.Context, r *appv1.PutEvidenceRequest) (*appv1.PutEvidenceReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if int64(len(s.uploads[r.Sha256])) != r.Offset {
		return nil, ErrCapture
	}
	s.uploads[r.Sha256] = append(s.uploads[r.Sha256], r.Data...)
	return &appv1.PutEvidenceReply{Sha256: r.Sha256, Size: r.Size, Received: int64(len(s.uploads[r.Sha256])), Complete: int64(len(s.uploads[r.Sha256])) == r.Size, ContentType: r.ContentType, RetainUntilUnixMs: s.until}, nil
}
func (s *rpcServer) Publish(ctx context.Context, r *appv1.PublishRequest) (*appv1.PublishReply, error) {
	if s.publicationGate != nil {
		select {
		case <-s.publicationGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p, e := appv1.DecodePublish(r)
	if e != nil {
		return nil, e
	}
	c, e := sensorrecord.ValidatePublication(p)
	if e != nil {
		return nil, e
	}
	ref, e := c.Evidence()
	if e != nil || ref == nil {
		return nil, ErrCapture
	}
	s.mu.Lock()
	data := bytes.Clone(s.uploads[ref.SHA256])
	s.mu.Unlock()
	if sensorrecord.ValidateMedia(c, data) != nil {
		return nil, ErrCapture
	}
	signed, e := s.publisher.Publish(ctx, p)
	if e != nil {
		return nil, e
	}
	raw, e := json.Marshal(signed)
	if e != nil {
		return nil, e
	}
	s.mu.Lock()
	s.requests = append(s.requests, proto.Clone(r).(*appv1.PublishRequest))
	s.mu.Unlock()
	return &appv1.PublishReply{Accepted: true, SignedRecordJson: raw}, nil
}
func testRPC(t *testing.T) (appv1.WorldAppServiceClient, *rpcServer) {
	t.Helper()
	now := time.Now().UTC()
	id := testidentity.NewWith(t, now, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	certs, e := wendycert.NewDevice(wendycert.Config{Tenant: id.Tenant, Roots: [][]byte{id.RootDER}})
	if e != nil {
		t.Fatal(e)
	}
	p, e := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: serviceorigin.Scope{Tenant: id.Tenant, Site: "office", Device: id.Principal, App: "com.example.sensorlink", Service: "bridge", DeploymentGeneration: 5, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: sensorrecord.ComponentType, Stream: "camera-b"}}}, Epoch: 31, Lifetime: time.Minute, Clock: rpcClock{now}, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = p.Close() })
	dir, e := os.MkdirTemp("", "ww-link-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "world.sock")
	listener, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	server := grpc.NewServer()
	fixture := &rpcServer{publisher: p, uploads: map[string][]byte{}, until: now.Add(time.Hour).UnixMilli()}
	appv1.RegisterWorldAppServiceServer(server, fixture)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, e := liveapp.Dial(socket)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return appv1.NewWorldAppServiceClient(conn), fixture
}

func TestTCPHTTPBridgePreservesCapturesAndDropsIncompleteFrames(t *testing.T) {
	for _, drop := range []bool{false, true} {
		name := "complete"
		if drop {
			name = "missing-chunk"
		}
		t.Run(name, func(t *testing.T) {
			cfg := testSourceConfig(t)
			if drop {
				cfg.Drop = &DropChunk{FrameSequence: 1, ChunkSequence: 1}
			}
			var count atomic.Uint64
			scene := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/v1/samples" || r.Header.Get("Authorization") != "Bearer "+testToken {
					t.Error("source escaped its read-only capture path")
					http.Error(w, "denied", 403)
					return
				}
				_ = json.NewEncoder(w).Encode(testSnapshot(t, count.Add(1)+16, cfg.Commission.BootID))
			}))
			defer scene.Close()
			api, rpc := testRPC(t)
			tcp, metadata, done := startSource(t, cfg, scene.URL)
			bridge := BridgeConfig{Commission: cfg.Commission, Bounds: cfg.Bounds, IsolatedSimulationNetwork: true, TCPAddress: tcp, MetadataURL: metadata, MetadataReadTokenFile: cfg.MetadataReadTokenFile, ChannelID: cfg.ChannelID}
			out, e := RunBridge(context.Background(), bridge, api)
			if e != nil || out.Status != "complete" {
				t.Fatalf("bridge failed: result=%+v error=%v", out, e)
			}
			var source sourceResult
			select {
			case source = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("source did not finish")
			}
			if source.err != nil {
				t.Fatal(source.err)
			}
			rpc.mu.Lock()
			defer rpc.mu.Unlock()
			want := 2
			if drop {
				want = 1
			}
			if len(rpc.requests) != want || len(out.Receipts) != want {
				t.Fatal("incomplete or duplicate frame reached publication")
			}
			for i, r := range rpc.requests {
				p, e := appv1.DecodePublish(r)
				if e != nil {
					t.Fatal(e)
				}
				c, e := sensorrecord.ValidatePublication(p)
				if e != nil {
					t.Fatal(e)
				}
				sample, _ := c.Sample()
				original := source.result.Frames[i].Metadata
				if drop {
					original = source.result.Frames[1].Metadata
				}
				originalCapture, _ := sensorrecord.Decode(original.CaptureJSON)
				if !bytes.Equal(c.SampleJSON, originalCapture.SampleJSON) || c.SceneBootID != originalCapture.SceneBootID || c.CapturedUnixNS != originalCapture.CapturedUnixNS || c.SimulationAnchorUnixNS != originalCapture.SimulationAnchorUnixNS || c.SensorLink.FrameSequence != original.FrameSequence || c.SensorLink.TimestampUS != original.TimestampUS || c.SensorLink.CaptureSequence != sample.Sequence || c.SensorLink.Evidence.SHA256 != original.JPEGSHA256 || digest(rpc.uploads[original.JPEGSHA256]) != original.JPEGSHA256 {
					t.Fatal("capture, changing calibration, wire identity, or exact JPEG was lost")
				}
				if sample.Sequence == uint64(c.SensorLink.FrameSequence) || out.Receipts[i].NativeSequence != sample.Sequence {
					t.Fatal("native and wire sequence were conflated")
				}
			}
			if drop && (out.DroppedFrames == 0 || out.Receipts[0].NativeSequence != 18 || source.result.ChunksOmitted != 1) {
				t.Fatal("dropped chunk did not suppress exactly its incomplete capture")
			}
			if !drop {
				a, _ := appv1.DecodePublish(rpc.requests[0])
				b, _ := appv1.DecodePublish(rpc.requests[1])
				ca, _ := sensorrecord.Decode(a.Payload)
				cb, _ := sensorrecord.Decode(b.Payload)
				sa, _ := ca.Sample()
				sb, _ := cb.Sample()
				if sa.Camera.CameraToSite[3] == sb.Camera.CameraToSite[3] || sa.CalibrationRevision == sb.CalibrationRevision || ca.SensorLink.CalibrationSHA256 == cb.SensorLink.CalibrationSHA256 {
					t.Fatal("moving camera calibration was frozen")
				}
			}
		})
	}
}

func TestLiveSourceRestartCannotPublishNewBoot(t *testing.T) {
	cfg := testSourceConfig(t)
	var count atomic.Uint64
	scene := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seq := count.Add(1) + 16
		boot := cfg.Commission.BootID
		if seq > 17 {
			boot = "restarted-scene"
		}
		_ = json.NewEncoder(w).Encode(testSnapshot(t, seq, boot))
	}))
	defer scene.Close()
	api, rpc := testRPC(t)
	tcp, metadata, done := startSource(t, cfg, scene.URL)
	bridge := BridgeConfig{Commission: cfg.Commission, Bounds: cfg.Bounds, IsolatedSimulationNetwork: true, TCPAddress: tcp, MetadataURL: metadata, MetadataReadTokenFile: cfg.MetadataReadTokenFile, ChannelID: cfg.ChannelID}
	out, e := RunBridge(context.Background(), bridge, api)
	if e == nil || out.Status == "complete" {
		t.Fatal("bridge silently survived scene boot replacement")
	}
	select {
	case source := <-done:
		if !errors.Is(source.err, livecapture.ErrBootChanged) {
			t.Fatalf("source did not reject changed scene boot: %v", source.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("source did not finish")
	}
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if len(rpc.requests) != 1 || len(out.Receipts) != 1 {
		t.Fatal("new scene boot was published or previous receipt lost")
	}
}

func TestConfigurationAndResultsContainNoCredentials(t *testing.T) {
	cfg := testSourceConfig(t)
	for _, change := range []func(*SourceConfig){func(c *SourceConfig) { c.IsolatedSimulationNetwork = false }, func(c *SourceConfig) { c.Bounds.MaxFrames = 65 }, func(c *SourceConfig) { c.Commission.AnchorUnixNS = 0 }, func(c *SourceConfig) { c.Commission.SensorID = "actuator" }, func(c *SourceConfig) { c.SceneURL = "http://user:password@scene:80" }, func(c *SourceConfig) { c.Drop = &DropChunk{FrameSequence: 0} }} {
		bad := cfg
		change(&bad)
		if _, e := bad.normalize(); !errors.Is(e, ErrConfig) {
			t.Fatal("unbounded or implicit network configuration accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(cfg)
	raw = append(raw[:len(raw)-1], []byte(`,"actuator_token":"secret"}`)...)
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadSourceConfig(path); !errors.Is(e, ErrConfig) {
		t.Fatal("unexpected actuator credential accepted")
	}
	if e := os.Chmod(cfg.SceneReadTokenFile, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := readToken(cfg.SceneReadTokenFile); !errors.Is(e, ErrConfig) {
		t.Fatal("nonprivate read token accepted")
	}
	out, e := RunSource(context.Background(), cfg)
	raw, _ = json.Marshal(out)
	if e == nil || bytes.Contains(raw, []byte(testToken)) || bytes.Contains(raw, []byte(cfg.SceneReadTokenFile)) || bytes.Contains(raw, []byte(cfg.SceneURL)) {
		t.Fatal("failure result leaked secret configuration")
	}
}

func TestMetadataClientTimeoutDoesNotWaitForUnboundedBody(t *testing.T) {
	cfg := testSourceConfig(t)
	f := testFrame(t, cfg, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	cfg.Bounds.RequestTimeoutMS = 30
	client := newMetadataClient(BridgeConfig{Commission: cfg.Commission, Bounds: cfg.Bounds, MetadataURL: server.URL}, testToken)
	defer client.http.CloseIdleConnections()
	start := time.Now()
	if _, err := client.fetch(context.Background(), received(f)); !errors.Is(err, ErrMetadata) || time.Since(start) > time.Second {
		t.Fatalf("unbounded metadata body was not refused promptly: %v", err)
	}
}

func TestPausedSourceDoesNotRefreshOrReissueOldCapture(t *testing.T) {
	cfg := testSourceConfig(t)
	api, rpc := testRPC(t)
	original := testSnapshot(t, 17, cfg.Commission.BootID)
	scene := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(original) }))
	defer scene.Close()
	tcp, metadata, done := startSource(t, cfg, scene.URL)
	bridge := BridgeConfig{Commission: cfg.Commission, Bounds: cfg.Bounds, IsolatedSimulationNetwork: true, TCPAddress: tcp, MetadataURL: metadata, MetadataReadTokenFile: cfg.MetadataReadTokenFile, ChannelID: cfg.ChannelID}
	out, err := RunBridge(context.Background(), bridge, api)
	if err == nil || out.Status == "complete" {
		t.Fatal("paused capture kept producing fresh wire frames")
	}
	select {
	case source := <-done:
		if !errors.Is(source.err, livecapture.ErrStale) || source.result.FramesSent != 1 {
			t.Fatalf("paused original capture did not expire: frames=%d error=%v", source.result.FramesSent, source.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("paused source did not finish")
	}
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if len(rpc.requests) != 1 || len(out.Receipts) != 1 || out.Receipts[0].CapturedUnixNS != original.SampleUnixMS*int64(time.Millisecond) {
		t.Fatal("paused source republished or rewrote original capture time")
	}
}

func TestBridgeWaitsForInitialListenerButNeverRetriesFailedHandshake(t *testing.T) {
	t.Run("delayed-listener", func(t *testing.T) {
		cfg := testSourceConfig(t)
		cfg.Bounds.MaxFrames = 1
		api, rpc := testRPC(t)
		scene := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(testSnapshot(t, 17, cfg.Commission.BootID))
		}))
		defer scene.Close()
		client, err := livecapture.New(livecapture.Config{URL: scene.URL, Token: testToken, RunID: cfg.Commission.RunID, Site: cfg.Commission.Site, Frame: cfg.Commission.Frame})
		if err != nil {
			t.Fatal(err)
		}
		cache := newCache(testToken)
		metadata := httptest.NewServer(cache)
		defer metadata.Close()
		// Bind without listening reserves a real TCP port while early dials
		// receive connection refusal. No port release/rebind race is needed.
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		file := os.NewFile(uintptr(fd), "delayed-test-listener")
		defer file.Close()
		if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
			t.Fatal(err)
		}
		addr, err := unix.Getsockname(fd)
		if err != nil {
			t.Fatal(err)
		}
		bridge := BridgeConfig{Commission: cfg.Commission, Bounds: cfg.Bounds, IsolatedSimulationNetwork: true, TCPAddress: fmt.Sprintf("127.0.0.1:%d", addr.(*unix.SockaddrInet4).Port), MetadataURL: metadata.URL, MetadataReadTokenFile: cfg.MetadataReadTokenFile, ChannelID: cfg.ChannelID}
		done := make(chan sourceResult, 1)
		started := make(chan struct{})
		go func() {
			close(started)
			result, err := RunBridge(context.Background(), bridge, api)
			done <- sourceResult{result, err}
		}()
		<-started
		select {
		case early := <-done:
			t.Fatalf("bridge did not wait for startup: %v", early.err)
		case <-time.After(100 * time.Millisecond):
		}
		if err := unix.Listen(fd, 1); err != nil {
			t.Fatal(err)
		}
		listener, err := net.FileListener(file)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		conn, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := serveConn(ctx, conn, client, cache, cfg); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-done:
			if got.err != nil || len(got.result.Receipts) != 1 {
				t.Fatalf("delayed listener did not produce a capture: %v", got.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("bridge did not finish")
		}
		rpc.mu.Lock()
		defer rpc.mu.Unlock()
		if len(rpc.requests) != 1 {
			t.Fatal("initial connection retry duplicated publication")
		}
	})
	t.Run("connected-handshake-failure", func(t *testing.T) {
		cfg := testSourceConfig(t)
		api, rpc := testRPC(t)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		var connections atomic.Int32
		serverDone := make(chan struct{})
		go func() {
			defer close(serverDone)
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				connections.Add(1)
				_ = conn.Close()
			}
		}()
		bridge := BridgeConfig{Commission: cfg.Commission, Bounds: cfg.Bounds, IsolatedSimulationNetwork: true, TCPAddress: listener.Addr().String(), MetadataURL: "http://127.0.0.1:1", MetadataReadTokenFile: cfg.MetadataReadTokenFile, ChannelID: cfg.ChannelID}
		out, err := RunBridge(context.Background(), bridge, api)
		_ = listener.Close()
		<-serverDone
		if err == nil || len(out.Receipts) != 0 || connections.Load() != 1 {
			t.Fatal("connected failed handshake was retried or produced a receipt")
		}
		rpc.mu.Lock()
		defer rpc.mu.Unlock()
		if len(rpc.requests) != 0 || len(rpc.uploads) != 0 {
			t.Fatal("failed handshake reached World mutation")
		}
	})
}

func TestFiniteBridgeCompletesFinalWireFrameAfterCoalescing(t *testing.T) {
	for _, final := range []int{4, 3} {
		t.Run(fmt.Sprint(final), func(t *testing.T) {
			cfg := testSourceConfig(t)
			cfg.Bounds.MaxFrames = final
			var count atomic.Uint64
			scene := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(testSnapshot(t, count.Add(1)+16, cfg.Commission.BootID))
			}))
			defer scene.Close()
			api, rpc := testRPC(t)
			gate := make(chan struct{})
			rpc.publicationGate = gate
			tcp, metadata, done := startSource(t, cfg, scene.URL)
			bridge := BridgeConfig{Commission: cfg.Commission, Bounds: cfg.Bounds, IsolatedSimulationNetwork: true, TCPAddress: tcp, MetadataURL: metadata, MetadataReadTokenFile: cfg.MetadataReadTokenFile, ChannelID: cfg.ChannelID}
			bridge.Bounds.MaxFrames = 4
			got := make(chan sourceResult, 1)
			go func() { out, err := RunBridge(context.Background(), bridge, api); got <- sourceResult{out, err} }()
			select {
			case source := <-done:
				if source.err != nil {
					t.Fatal(source.err)
				}
			case <-time.After(3 * time.Second):
				close(gate)
				t.Fatal("finite source did not finish")
			}
			close(gate)
			select {
			case result := <-got:
				if final == 3 {
					if result.err == nil || result.result.Status == "complete" {
						t.Fatal("premature EOF counted as final frame")
					}
					return
				}
				if result.err != nil || result.result.Status != "complete" || len(result.result.Receipts) != 2 || result.result.DroppedFrames != 2 {
					t.Fatalf("coalesced finite stream failed: count=%d dropped=%d err=%v", len(result.result.Receipts), result.result.DroppedFrames, result.err)
				}
				for i, want := range []uint32{1, 4} {
					var signed serviceorigin.SignedRecord
					_ = json.Unmarshal(result.result.Receipts[i].SignedRecordJSON, &signed)
					var record serviceorigin.Record
					_ = json.Unmarshal(signed.Record, &record)
					capture, err := sensorrecord.Decode(record.Publication.Payload)
					if err != nil || capture.SensorLink.FrameSequence != want {
						t.Fatal("coalescing rewrote wire sequence")
					}
				}
			case <-time.After(3 * time.Second):
				t.Fatal("bridge waited beyond final wire frame")
			}
		})
	}
}
