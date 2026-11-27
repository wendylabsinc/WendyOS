package sensorlinkbridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/sensorlink"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func fixture(t *testing.T) (Config, *sensorlink.Frame, time.Time) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Unix(1700000000, 0)
	s := sensing.Sample{SchemaVersion: sensing.Schema, ID: "wire/camera-a/1", SiteID: "office", Frame: world.FrameRef{ID: "floor", Revision: "1"}, SensorID: "camera-a", Kind: "camera", Sequence: 1, SampleNS: 250000000, RunID: "wire", ScenarioVersion: "scene-1", Seed: 7, PackageSHA256: strings.Repeat("a", 64), SensorModel: "mujoco-rgb", PositionM: []float64{1, 2, 3}, UncertaintyM: .1, CalibrationRevision: "cal-1", Camera: &sensing.Camera{Width: 64, Height: 48, FX: 40, FY: 40, CX: 32, CY: 24, CameraToSite: []float64{1, 0, 0, 1, 0, 1, 0, 2, 0, 0, 1, 3, 0, 0, 0, 1}, CalibrationValidForNS: int64(10 * time.Second)}, Evidence: &sensing.EvidenceRef{SHA256: strings.Repeat("b", 64), Size: 100, ContentType: "image/png"}}
	g := authority.SignedGrant{Grant: authority.Grant{Domain: "simulation", Tenant: "tenant", Site: "office", PublicKey: pub, Source: world.Source{Device: "gateway", App: "sensorlink", Service: "camera-a", Stream: "samples"}}}
	c := Config{Publisher: client.Config{URL: "http://127.0.0.1:1", Audience: "node-1", Grant: g, PrivateKey: key, AllowInsecureSimulation: true}, Endpoint: "127.0.0.1:50060", ChannelID: 1, SampleTemplate: s, AnchorUnixUS: anchor.UnixMicro(), TimestampClock: "unix-us", ClockErrorUS: 1000, TTLMS: 5000, AllowInsecureSimulation: true}
	im := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			im.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var b bytes.Buffer
	if err = jpeg.Encode(&b, im, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	f := &sensorlink.Frame{ChannelID: 1, Seq: 0, TsUs: uint64(anchor.Add(250 * time.Millisecond).UnixMicro()), Flags: 1, Payload: b.Bytes()}
	return c, f, anchor.Add(300 * time.Millisecond)
}

func TestWireFramesPreserveCaptureAndCalibrationExpiry(t *testing.T) {
	cfg, frame, now := fixture(t)
	converter, err := New(cfg, 3)
	if err != nil {
		t.Fatal(err)
	}
	first, err := converter.Convert(frame, now)
	if err != nil {
		t.Fatal(err)
	}
	s, err := sensing.FromComponent(first)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(frame.Payload)
	if s.SensorLink == nil || s.SensorLink.FrameSequence != 0 || s.SensorLink.TimestampUS != frame.TsUs || s.Evidence.SHA256 != hex.EncodeToString(h[:]) || s.Evidence.ContentType != "image/jpeg" || !first.Timing.SampleTime.Equal(time.UnixMicro(int64(frame.TsUs))) || first.Timing.ErrorBound != time.Millisecond || first.Version.Epoch != 3 || first.Provenance.Origin != world.Simulated {
		t.Fatal("wire metadata or timing lost", first)
	}
	next := *frame
	next.Seq = 2
	next.TsUs += 250000
	second, err := converter.Convert(&next, now.Add(250*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	s2, err := sensing.FromComponent(second)
	if err != nil {
		t.Fatal(err)
	}
	if second.Version.Sequence != 2 || s2.SensorLink.FrameSequence != 2 || s2.SampleNS != 500000000 {
		t.Fatal("gap not retained", s2)
	}
	if !first.Timing.SampleTime.Add(time.Duration(s.Camera.CalibrationValidForNS)).Equal(second.Timing.SampleTime.Add(time.Duration(s2.Camera.CalibrationValidForNS))) {
		t.Fatal("calibration expiry renewed")
	}
	for _, mutate := range []func(*world.Component){func(c *world.Component) { c.Timing.SampleTime = c.Timing.SampleTime.Add(time.Microsecond) }, func(c *world.Component) { c.Timing.ErrorBound = 0 }, func(c *world.Component) { c.Timing.ErrorKnown = false }} {
		bad := second
		mutate(&bad)
		if _, err = sensing.FromComponent(bad); err == nil {
			t.Fatal("accepted changed capture binding")
		}
	}
}

func TestFrameRejectionDoesNotAdvanceAndSequenceWrapIsExplicit(t *testing.T) {
	cfg, frame, now := fixture(t)
	for _, test := range []struct {
		name   string
		change func(*sensorlink.Frame)
		at     time.Time
	}{
		{"wrong channel", func(f *sensorlink.Frame) { f.ChannelID++ }, now},
		{"future", func(f *sensorlink.Frame) { f.TsUs = uint64(now.Add(time.Second).UnixMicro()) }, now},
		{"stale", func(f *sensorlink.Frame) {}, now.Add(6 * time.Second)},
		{"bad jpeg", func(f *sensorlink.Frame) { f.Payload = []byte("bad") }, now},
		{"truncated jpeg", func(f *sensorlink.Frame) { f.Payload = f.Payload[:len(f.Payload)-50] }, now},
		{"unknown flags", func(f *sensorlink.Frame) { f.Flags = 8 }, now},
		{"timestamp overflow", func(f *sensorlink.Frame) { f.TsUs = math.MaxUint64 }, now},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, err := New(cfg, 1)
			if err != nil {
				t.Fatal(err)
			}
			bad := *frame
			test.change(&bad)
			if _, err = c.Convert(&bad, test.at); err == nil {
				t.Fatal("bad frame accepted")
			}
			if _, err = c.Convert(frame, now); err != nil {
				t.Fatal("failure advanced state", err)
			}
		})
	}
	c, err := New(cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	first := *frame
	first.Seq = math.MaxUint32
	if _, err = c.Convert(&first, now); err != nil {
		t.Fatal(err)
	}
	next := *frame
	next.TsUs += 100000
	if _, err = c.Convert(&next, now.Add(100*time.Millisecond)); err != nil {
		t.Fatal("uint32 rollover rejected", err)
	}
	if _, err = c.Convert(&next, now.Add(100*time.Millisecond)); err == nil {
		t.Fatal("duplicate accepted")
	}
	next.Seq = math.MaxUint32
	next.TsUs += 100000
	if _, err = c.Convert(&next, now.Add(200*time.Millisecond)); err == nil {
		t.Fatal("backward serial sequence accepted")
	}
}

func TestManifestClockAndFixedCalibration(t *testing.T) {
	cfg, frame, now := fixture(t)
	manifest := &pb.SensorManifest{Sensors: []*pb.SensorDescriptor{{ChannelId: 1, Name: "camera-a", Format: &pb.SensorDescriptor_Video{Video: &pb.VideoFormat{Codec: pb.VideoFormat_MJPEG, Width: 64, Height: 48, Fps: 30}}}}}
	if err := cfg.ValidateManifest(manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Sensors[0].GetVideo().Codec = pb.VideoFormat_H264
	if err := cfg.ValidateManifest(manifest); err == nil {
		t.Fatal("unsupported codec accepted")
	}
	for _, change := range []func(*Config){func(c *Config) { c.TimestampClock = "uptime-us" }, func(c *Config) { c.ClockErrorUS = 0 }, func(c *Config) { c.AllowInsecureSimulation = false }, func(c *Config) { c.Publisher.Grant.Grant.Domain = "physical" }, func(c *Config) { c.Publisher.Grant.Grant.Source.Service = "camera-b" }} {
		bad := cfg
		bad.Publisher.Grant.Grant = cfg.Publisher.Grant.Grant
		change(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatal("bad scope accepted")
		}
	}
	converter, err := New(cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	frame.TsUs += 11000000
	if _, err = converter.Convert(frame, now.Add(11*time.Second)); err == nil {
		t.Fatal("expired fixed calibration refreshed")
	}
}

type fakePublisher struct {
	uploadErr error
	upload    evidence.PutResponse
	got       []byte
	meta      evidence.GetResponse
	published []world.Component
	uploads   int
	hook      func()
}

func (p *fakePublisher) UploadEvidence(context.Context, []byte, string, time.Time) (evidence.PutResponse, error) {
	p.uploads++
	if p.hook != nil {
		p.hook()
	}
	return p.upload, p.uploadErr
}
func (p *fakePublisher) DownloadEvidence(context.Context, string, int64) ([]byte, evidence.GetResponse, error) {
	return p.got, p.meta, nil
}
func (p *fakePublisher) Publish(_ context.Context, c world.Component) error {
	p.published = append(p.published, c)
	return nil
}

func TestPublicationPreservesBytesAndCannotRenewStaleMedia(t *testing.T) {
	cfg, frame, now := fixture(t)
	conv, err := New(cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	component, err := conv.Convert(frame, now)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := sensing.FromComponent(component)
	ref := s.Evidence
	for _, reuse := range []bool{false, true} {
		p := &fakePublisher{upload: evidence.PutResponse{SHA256: ref.SHA256, ContentType: ref.ContentType, Size: ref.Size, Received: ref.Size, Complete: true, RetainUntilUnixMS: component.Timing.SampleTime.Add(2 * time.Second).UnixMilli()}}
		if reuse {
			p.uploadErr = errors.New("owned by another writer")
			p.got = frame.Payload
			p.meta = evidence.GetResponse{SHA256: ref.SHA256, ContentType: ref.ContentType, Size: ref.Size, RetainUntilUnixMS: p.upload.RetainUntilUnixMS}
		}
		if err = Publish(context.Background(), p, component, frame.Payload, func() time.Time { return now }); err != nil {
			t.Fatal(err)
		}
		if len(p.published) != 1 || p.published[0].TTL != 2*time.Second || !p.published[0].Timing.SampleTime.Equal(component.Timing.SampleTime) {
			t.Fatal("retention/timing changed", p.published)
		}
	}
	for _, fault := range []string{"bytes", "slow", "clock rollback", "wrong reused type"} {
		t.Run(fault, func(t *testing.T) {
			at := now
			p := &fakePublisher{upload: evidence.PutResponse{SHA256: ref.SHA256, ContentType: ref.ContentType, Size: ref.Size, Received: ref.Size, Complete: true, RetainUntilUnixMS: now.Add(time.Hour).UnixMilli()}}
			data := frame.Payload
			switch fault {
			case "bytes":
				data = []byte("different")
			case "slow":
				p.hook = func() { at = at.Add(6 * time.Second) }
			case "clock rollback":
				p.hook = func() { at = at.Add(-time.Millisecond) }
			case "wrong reused type":
				p.uploadErr = errors.New("owned")
				p.got = data
				p.meta = evidence.GetResponse{SHA256: ref.SHA256, ContentType: "image/png", Size: ref.Size, RetainUntilUnixMS: p.upload.RetainUntilUnixMS}
			}
			if err = Publish(context.Background(), p, component, data, func() time.Time { return at }); err == nil || len(p.published) != 0 {
				t.Fatal("bad upload published", err)
			}
		})
	}
}

func TestConfigDecodeRequiresTemplateNumbers(t *testing.T) {
	cfg, _, _ := fixture(t)
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Decode(raw); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(raw, &fields)
	var sample map[string]json.RawMessage
	json.Unmarshal(fields["sample_template"], &sample)
	delete(sample, "seed")
	fields["sample_template"], _ = json.Marshal(sample)
	raw, _ = json.Marshal(fields)
	if _, err = Decode(raw); err == nil {
		t.Fatal("missing template seed accepted")
	}
}
