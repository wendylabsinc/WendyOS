package livesensorlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/cameraframe"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type nativeReply struct {
	frame cameraframe.Frame
	err   error
}
type nativeTestAPI struct {
	appv1.WorldAppServiceClient
	replies                   []nativeReply
	reads, uploads, publishes int
	losePublishAck            bool
	wrongRequest              bool
}
type nativeTestStream struct {
	grpc.ClientStream
	rows  []*appv1.SensorFrameChunk
	index int
}

func (s *nativeTestStream) Recv() (*appv1.SensorFrameChunk, error) {
	if s.index == len(s.rows) {
		return nil, io.EOF
	}
	r := s.rows[s.index]
	s.index++
	return r, nil
}
func (a *nativeTestAPI) GetSensorFrame(ctx context.Context, request *appv1.GetSensorFrameRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[appv1.SensorFrameChunk], error) {
	if request.ComponentType != sensorrecord.ComponentType || request.Stream != "camera-b" {
		a.wrongRequest = true
		return nil, status.Error(codes.PermissionDenied, "scope")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	i := a.reads
	a.reads++
	if len(a.replies) == 0 {
		return nil, status.Error(codes.Unavailable, "no frame")
	}
	r := a.replies[min(i, len(a.replies)-1)]
	if r.err != nil {
		return nil, r.err
	}
	m, err := cameraframe.EncodeMetadata(r.frame.Metadata)
	if err != nil {
		return nil, err
	}
	s := &nativeTestStream{}
	for off := 0; off < len(r.frame.JPEG); off += cameraframe.MaxChunkBytes {
		end := min(off+cameraframe.MaxChunkBytes, len(r.frame.JPEG))
		s.rows = append(s.rows, &appv1.SensorFrameChunk{MetadataJson: bytes.Clone(m), Offset: int64(off), Data: bytes.Clone(r.frame.JPEG[off:end])})
	}
	return s, nil
}
func (a *nativeTestAPI) PutEvidence(ctx context.Context, r *appv1.PutEvidenceRequest, o ...grpc.CallOption) (*appv1.PutEvidenceReply, error) {
	a.uploads++
	if a.WorldAppServiceClient == nil {
		return nil, status.Error(codes.Unavailable, "unexpected mutation")
	}
	return a.WorldAppServiceClient.PutEvidence(ctx, r, o...)
}
func (a *nativeTestAPI) Publish(ctx context.Context, r *appv1.PublishRequest, o ...grpc.CallOption) (*appv1.PublishReply, error) {
	a.publishes++
	if a.WorldAppServiceClient == nil {
		return nil, status.Error(codes.Unavailable, "unexpected mutation")
	}
	reply, err := a.WorldAppServiceClient.Publish(ctx, r, o...)
	if err == nil && a.losePublishAck {
		return nil, status.Error(codes.Unavailable, "ack lost after commit")
	}
	return reply, err
}

func nativeTestConfig(t *testing.T) (SourceConfig, NativeBridgeConfig) {
	t.Helper()
	source := testSourceConfig(t)
	cfg := NativeBridgeConfig{Commission: source.Commission, Bounds: source.Bounds, IsolatedSimulationNetwork: true, MetadataURL: "http://127.0.0.1:1", MetadataReadTokenFile: source.MetadataReadTokenFile, ChannelID: source.ChannelID, SourceID: "sensorlink:spiffe://wendy.sh/tenant/test/device/gateway:4", PollIntervalMS: 125}
	return source, cfg
}
func nativePacket(t *testing.T, p preparedFrame, cfg NativeBridgeConfig) cameraframe.Frame {
	t.Helper()
	m := p.metadata
	f, err := cameraframe.NewFrame(cameraframe.Metadata{Version: 1, SourceID: cfg.SourceID, SourceSessionID: "native-stream-1", Transport: "tcp", ChannelID: m.ChannelID, FrameSequence: m.FrameSequence, TimestampUS: m.TimestampUS, TimestampBasis: "unknown", Flags: m.Flags, Receipt: cameraframe.Receipt{BootID: "gateway-boot", BeforeBootNS: 10, ReceivedBootNS: 11, AfterBootNS: 12, GatewayWallUnixNS: 3}, Codec: "mjpeg", Width: p.width, Height: p.height, CalibrationJSON: []byte(`{"revision":"uninterpreted-native"}`)}, p.jpeg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func nativeMetadataServer(t *testing.T, frames ...preparedFrame) (string, *atomic.Int32) {
	t.Helper()
	cache := newCache(testToken)
	for _, f := range frames {
		if err := cache.put(f.metadata); err != nil {
			t.Fatal(err)
		}
	}
	count := &atomic.Int32{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count.Add(1); cache.ServeHTTP(w, r) }))
	t.Cleanup(s.Close)
	return s.URL, count
}

func TestNativeBridgePreservesOriginalCaptureDeduplicatesAndCompletesFinalSequence(t *testing.T) {
	client, rpc := testRPC(t)
	source, cfg := nativeTestConfig(t)
	cfg.Bounds.MaxFrames = 3
	first := testFrame(t, source, 1)
	time.Sleep(2 * time.Millisecond)
	last := testFrame(t, source, 3)
	url, gets := nativeMetadataServer(t, first, last)
	cfg.MetadataURL = url
	f1, f3 := nativePacket(t, first, cfg), nativePacket(t, last, cfg)
	api := &nativeTestAPI{WorldAppServiceClient: client, replies: []nativeReply{{err: status.Error(codes.Unavailable, "not received")}, {frame: f1}, {frame: f1}, {frame: f3}}}
	out, err := RunNativeBridge(context.Background(), cfg, api)
	if err != nil || out.Status != "complete" || out.Mode != "native-bridge" || out.DuplicatePolls != 1 || len(out.Receipts) != 2 || len(out.NativeFrames) != 2 || api.reads != 4 || api.publishes != 2 || gets.Load() != 2 || api.wrongRequest {
		t.Fatalf("native bridge failed or republished a duplicate: %v %+v", err, out)
	}
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	for i, original := range []preparedFrame{first, last} {
		p, err := appv1.DecodePublish(rpc.requests[i])
		if err != nil {
			t.Fatal(err)
		}
		capture, err := sensorrecord.ValidatePublication(p)
		if err != nil {
			t.Fatal(err)
		}
		old, err := sensorrecord.Decode(original.metadata.CaptureJSON)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(capture.SampleJSON, old.SampleJSON) || capture.SceneBootID != old.SceneBootID || capture.CapturedUnixNS != old.CapturedUnixNS || capture.SimulationAnchorUnixNS != old.SimulationAnchorUnixNS || p.Provenance.Origin != world.Simulated || p.Timing.ClockDomain != "simulation:"+cfg.Commission.RunID {
			t.Fatal("native receipt rewrote original simulation ancestry")
		}
		wire := capture.SensorLink
		if wire == nil || wire.FrameSequence != original.metadata.FrameSequence || wire.TimestampUS != original.metadata.TimestampUS || wire.Evidence.SHA256 != original.metadata.JPEGSHA256 || !bytes.Equal(rpc.uploads[wire.Evidence.SHA256], original.jpeg) {
			t.Fatal("original native JPEG or mapping changed")
		}
		m := out.NativeFrames[i]
		if m.ProofSHA256 != out.Receipts[i].ProofSHA256 || m.Metadata.TimestampBasis != "unknown" || m.Metadata.Receipt.GatewayWallUnixNS != 3 || m.Metadata.JPEGSHA256 != wire.Evidence.SHA256 || m.Metadata.SourceID != cfg.SourceID {
			t.Fatal("native receipt join changed")
		}
	}
}

func TestNativeBridgeRefusesWrongSourceAndMetadataBeforeMutation(t *testing.T) {
	for _, mode := range []string{"source", "channel", "timestamp", "flags", "jpeg", "denied"} {
		t.Run(mode, func(t *testing.T) {
			source, cfg := nativeTestConfig(t)
			original := testFrame(t, source, 1)
			cfg.MetadataURL, _ = nativeMetadataServer(t, original)
			f := nativePacket(t, original, cfg)
			switch mode {
			case "source":
				f.Metadata.SourceID = "sensorlink:another:4"
			case "channel":
				f.Metadata.ChannelID++
			case "timestamp":
				f.Metadata.TimestampUS++
			case "flags":
				f.Metadata.Flags = 0
			case "jpeg":
				other := testFrame(t, source, 2)
				f, _ = cameraframe.NewFrame(f.Metadata, other.jpeg)
			}
			r := nativeReply{frame: f}
			if mode == "denied" {
				r.err = status.Error(codes.PermissionDenied, "denied")
			}
			api := &nativeTestAPI{replies: []nativeReply{r}}
			out, err := RunNativeBridge(context.Background(), cfg, api)
			if err == nil || out.Status == "complete" || api.uploads != 0 || api.publishes != 0 || api.reads != 1 {
				t.Fatal("mismatched native frame reached mutation or retried", mode, err)
			}
		})
	}
}

func TestNativeBridgeSessionAndSameSequenceConflictsStayTerminal(t *testing.T) {
	for _, mode := range []string{"session", "timestamp", "digest", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			client, rpc := testRPC(t)
			source, cfg := nativeTestConfig(t)
			first := testFrame(t, source, 1)
			cfg.MetadataURL, _ = nativeMetadataServer(t, first)
			f := nativePacket(t, first, cfg)
			next := f.Clone()
			switch mode {
			case "session":
				next.Metadata.SourceSessionID = "replacement"
			case "timestamp":
				next.Metadata.TimestampUS++
			case "digest":
				other := testFrame(t, source, 2)
				next, _ = cameraframe.NewFrame(next.Metadata, other.jpeg)
			}
			r := nativeReply{frame: next}
			if mode == "unavailable" {
				r.err = status.Error(codes.Unavailable, "disconnected")
			}
			api := &nativeTestAPI{WorldAppServiceClient: client, replies: []nativeReply{{frame: f}, r}}
			out, err := RunNativeBridge(context.Background(), cfg, api)
			if err == nil || out.Status == "complete" || len(out.Receipts) != 1 || api.publishes != 1 || api.reads != 2 {
				t.Fatal("source change was retried or published", mode, err)
			}
			rpc.mu.Lock()
			n := len(rpc.requests)
			rpc.mu.Unlock()
			if n != 1 {
				t.Fatal("unexpected mutation count", n)
			}
		})
	}
}

func TestNativeBridgeUnknownMutationDoesNotRetryOrClaimFinalFrame(t *testing.T) {
	client, rpc := testRPC(t)
	source, cfg := nativeTestConfig(t)
	cfg.Bounds.MaxFrames = 1
	f := testFrame(t, source, 1)
	cfg.MetadataURL, _ = nativeMetadataServer(t, f)
	api := &nativeTestAPI{WorldAppServiceClient: client, replies: []nativeReply{{frame: nativePacket(t, f, cfg)}}, losePublishAck: true}
	out, err := RunNativeBridge(context.Background(), cfg, api)
	if !errors.Is(err, liveapp.ErrUncertain) || out.Status != "unknown_outcome" || len(out.Receipts) != 0 || len(out.NativeFrames) != 0 || api.reads != 1 || api.publishes != 1 {
		t.Fatal("ambiguous final publication retried or acknowledged", err)
	}
	rpc.mu.Lock()
	n := len(rpc.requests)
	rpc.mu.Unlock()
	if n != 1 {
		t.Fatal("test did not commit before losing acknowledgment")
	}
}

func TestNativeBridgeInitialAcquisitionAndPausedLatestStayBounded(t *testing.T) {
	source, cfg := nativeTestConfig(t)
	_ = source
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	api := &nativeTestAPI{}
	out, err := RunNativeBridge(ctx, cfg, api)
	if !errors.Is(err, context.DeadlineExceeded) || out.Status != "failed" || api.reads != 1 || api.publishes != 0 {
		t.Fatal("first-frame wait ignored caller bound", err)
	}
	client, _ := testRPC(t)
	source, cfg = nativeTestConfig(t)
	cfg.Bounds.DurationMS = 700
	f := testFrame(t, source, 1)
	cfg.MetadataURL, _ = nativeMetadataServer(t, f)
	api = &nativeTestAPI{WorldAppServiceClient: client, replies: []nativeReply{{frame: nativePacket(t, f, cfg)}}}
	out, err = RunNativeBridge(context.Background(), cfg, api)
	if !errors.Is(err, context.DeadlineExceeded) || out.Status == "complete" || len(out.Receipts) != 1 || api.publishes != 1 || out.DuplicatePolls == 0 {
		t.Fatal("paused original was renewed or final sequence invented", err)
	}
}

func TestNativeBridgeConfigIsSeparateAndExact(t *testing.T) {
	_, cfg := nativeTestConfig(t)
	cfg.PollIntervalMS = 0
	p := filepath.Join(t.TempDir(), "native.json")
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadNativeBridgeConfig(p)
	if err != nil || got.PollIntervalMS != 250 {
		t.Fatal("native config failed", err)
	}
	for _, mutate := range []func(*NativeBridgeConfig){func(c *NativeBridgeConfig) { c.IsolatedSimulationNetwork = false }, func(c *NativeBridgeConfig) { c.SourceID = "/dev/video96" }, func(c *NativeBridgeConfig) { c.ChannelID++ }, func(c *NativeBridgeConfig) { c.PollIntervalMS = 1 }, func(c *NativeBridgeConfig) { c.MetadataURL = "http://user:secret@source:80" }} {
		bad := cfg
		mutate(&bad)
		if _, err := bad.normalize(); !errors.Is(err, ErrConfig) {
			t.Fatal("invalid native config accepted")
		}
	}
	raw = append(raw[:len(raw)-1], []byte(`,"tcp_address":"source:50060"}`)...)
	if err := os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNativeBridgeConfig(p); !errors.Is(err, ErrConfig) {
		t.Fatal("direct TCP config accepted by native bridge")
	}
}
