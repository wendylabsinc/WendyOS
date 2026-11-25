package livecapture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var testPhysical = time.Unix(1_794_400_000, 0).UTC()

func captureFixture(t *testing.T, sensor string, sequence uint64) (Capture, Config) {
	t.Helper()
	frame := world.FrameRef{ID: "office-floor", Revision: "test1"}
	sample := sensing.Sample{SchemaVersion: sensing.Schema, SiteID: "studio-01", Frame: frame, SensorID: sensor, Kind: sensorKind(sensor), Sequence: sequence, SampleNS: int64(time.Second + time.Duration(sequence)*100*time.Millisecond), RunID: "capture-test", ScenarioVersion: "furnished-office-fusion-v1", Seed: 7, PackageSHA256: strings.Repeat("a", 64), SensorModel: "test-source", PositionM: []float64{0, 0, 0}, UncertaintyM: .02, CalibrationRevision: "office-test"}
	sample.ID = sample.RunID + "/" + sensor + "/" + stringNumber(sequence)
	var media []byte
	switch sample.Kind {
	case "camera":
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
			t.Fatal(err)
		}
		media = encoded.Bytes()
		sample.Camera = &sensing.Camera{Width: 2, Height: 2, FX: 2, FY: 2, CX: 1, CY: 1, CameraToSite: []float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}
	case "audio":
		media = make([]byte, 4044)
		copy(media, "RIFF")
		binary.LittleEndian.PutUint32(media[4:], uint32(len(media)-8))
		copy(media[8:], "WAVEfmt ")
		binary.LittleEndian.PutUint32(media[16:], 16)
		binary.LittleEndian.PutUint16(media[20:], 1)
		binary.LittleEndian.PutUint16(media[22:], 1)
		binary.LittleEndian.PutUint32(media[24:], 8000)
		binary.LittleEndian.PutUint32(media[28:], 16000)
		binary.LittleEndian.PutUint16(media[32:], 2)
		binary.LittleEndian.PutUint16(media[34:], 16)
		copy(media[36:], "data")
		binary.LittleEndian.PutUint32(media[40:], 4000)
		sample.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(250 * time.Millisecond)}
	case "gas":
		sample.Gas = &sensing.Gas{ValuePPM: 80, UncertaintyPPM: 1.5, Species: "synthetic-test-gas"}
	case "lidar":
		sample.Lidar = &sensing.Lidar{PointsM: [][3]float64{{1, .25, 1}, {2, .65, -1}}, MaxRangeM: 6.1}
	}
	if len(media) > 0 {
		digest := sha256.Sum256(media)
		contentType := "image/png"
		if sample.Kind == "audio" {
			contentType = "audio/wav"
		}
		sample.Evidence = &sensing.EvidenceRef{SHA256: hex.EncodeToString(digest[:]), ContentType: contentType, Size: int64(len(media))}
	}
	raw, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	// Preserve noncanonical original source JSON through Decode and Publish.
	raw = append(append([]byte("\n "), raw...), '\n')
	anchor := int64(1_767_225_600_000_000_000)
	return Capture{Capture: sensorrecord.Capture{Version: 1, SceneBootID: "office-boot", CapturedUnixNS: testPhysical.Add(time.Duration(sequence) * time.Millisecond).UnixNano(), SimulationAnchorUnixNS: anchor, SampleJSON: raw}, Media: media}, Config{RunID: sample.RunID, Site: sample.SiteID, Frame: frame, SensorID: sensor, SimulationAnchorUnixNS: anchor, DurationMS: 3000, MaxSamples: 1, PollIntervalMS: 50, TTLMS: 3000, RequestTimeoutMS: 1500}
}

func stringNumber(value uint64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func TestAllKindsPreserveOriginalBytesAndInlineData(t *testing.T) {
	for _, sensor := range []string{"camera-a", "camera-b", "people-front", "people-oblique", "gas-1", "audio-1", "lidar-atlas"} {
		t.Run(sensor, func(t *testing.T) {
			capture, cfg := captureFixture(t, sensor, 1)
			raw, _ := json.Marshal(capture)
			got, err := Decode(raw, cfg)
			if err != nil || !bytes.Equal(got.Capture.SampleJSON, capture.Capture.SampleJSON) || !bytes.Equal(got.Media, capture.Media) || got.Capture.CapturedUnixNS != capture.Capture.CapturedUnixNS {
				t.Fatal("original capture changed", err)
			}
			if sensor == "gas-1" || sensor == "lidar-atlas" {
				capture.Media = []byte("invented evidence")
			} else {
				capture.Media[0] ^= 1
			}
			raw, _ = json.Marshal(capture)
			if _, err = Decode(raw, cfg); !errors.Is(err, liveapp.ErrCapture) {
				t.Fatal("changed/extra media accepted", err)
			}
		})
	}
}

func TestCaptureBindsSourceScopeAndKind(t *testing.T) {
	for _, field := range []string{"sensor", "run", "site", "frame", "anchor", "kind"} {
		t.Run(field, func(t *testing.T) {
			capture, cfg := captureFixture(t, "lidar-atlas", 1)
			switch field {
			case "sensor":
				cfg.SensorID = "lidar-iris"
			case "run":
				cfg.RunID = "other"
			case "site":
				cfg.Site = "other"
			case "frame":
				cfg.Frame.Revision = "other"
			case "anchor":
				cfg.SimulationAnchorUnixNS++
			case "kind":
				sample, _ := capture.Capture.Sample()
				sample.Lidar, sample.Gas, sample.Kind = nil, &sensing.Gas{ValuePPM: 7, Species: "test"}, "gas"
				capture.Capture.SampleJSON, _ = json.Marshal(sample)
			}
			raw, _ := json.Marshal(capture)
			if _, err := Decode(raw, cfg); !errors.Is(err, liveapp.ErrCapture) {
				t.Fatal("changed commissioning accepted", err)
			}
		})
	}
}

type captureClock struct{}

func (captureClock) Now() (trustedtime.Interval, error) {
	return trustedtime.Interval{Earliest: testPhysical, Latest: testPhysical.Add(time.Millisecond), BootID: "test", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test"}, nil
}

type captureAPI struct {
	appv1.WorldAppServiceClient
	publisher          *serviceorigin.Publisher
	publishes, uploads int
	lostAck            bool
	requests           []serviceorigin.PublishRequest
	media              []byte
}

func (a *captureAPI) PutEvidence(_ context.Context, p *appv1.PutEvidenceRequest, _ ...grpc.CallOption) (*appv1.PutEvidenceReply, error) {
	a.uploads++
	a.media = append(a.media, p.Data...)
	return &appv1.PutEvidenceReply{Sha256: p.Sha256, Size: p.Size, ContentType: p.ContentType, Received: p.Offset + int64(len(p.Data)), Complete: p.Offset+int64(len(p.Data)) == p.Size, RetainUntilUnixMs: testPhysical.Add(time.Minute).UnixMilli()}, nil
}

func (a *captureAPI) Publish(ctx context.Context, p *appv1.PublishRequest, _ ...grpc.CallOption) (*appv1.PublishReply, error) {
	a.publishes++
	request, err := appv1.DecodePublish(p)
	if err != nil {
		return nil, err
	}
	a.requests = append(a.requests, request)
	proof, err := a.publisher.Publish(ctx, request)
	if err != nil {
		return nil, err
	}
	if a.lostAck {
		return nil, status.Error(codes.Unavailable, "test acknowledgment lost")
	}
	raw, err := json.Marshal(proof)
	return &appv1.PublishReply{Accepted: true, SignedRecordJson: raw}, err
}

func newAPI(t *testing.T, cfg Config) *captureAPI {
	t.Helper()
	identity := testidentity.NewWith(t, testPhysical, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	certificates, err := wendycert.NewDevice(wendycert.Config{Tenant: identity.Tenant, Roots: [][]byte{identity.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	scope := serviceorigin.Scope{Tenant: identity.Tenant, Site: cfg.Site, Device: identity.Principal, App: "com.example.capture", Service: cfg.SensorID, DeploymentGeneration: 1, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: sensorrecord.ComponentType, Stream: cfg.SensorID}}}
	publisher, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: scope, Epoch: 1, Lifetime: time.Minute, Clock: captureClock{}, Certificates: certificates, ChainDER: identity.ChainDER, DeviceKey: identity.Key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publisher.Close() })
	return &captureAPI{publisher: publisher}
}

func TestHTTPPublisherUploadsOnlyReferencedMedia(t *testing.T) {
	for _, sensor := range []string{"camera-a", "camera-b", "people-front", "people-oblique", "audio-1", "gas-1", "lidar-atlas"} {
		t.Run(sensor, func(t *testing.T) {
			capture, cfg := captureFixture(t, sensor, 1)
			raw, _ := json.Marshal(capture)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/captures/"+sensor || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 32) {
					t.Error("source request changed")
				}
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			cfg.SceneURL, cfg.ReadTokenFile = server.URL, filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(cfg.ReadTokenFile, []byte(strings.Repeat("t", 32)), 0600); err != nil {
				t.Fatal(err)
			}
			api := newAPI(t, cfg)
			var receipts []liveapp.Receipt
			err := Run(context.Background(), cfg, api, func(r liveapp.Receipt) error { receipts = append(receipts, r); return nil })
			if err != nil || api.publishes != 1 || len(receipts) != 1 {
				t.Fatal(err, api.publishes, len(receipts))
			}
			if (api.uploads > 0) != (len(capture.Media) > 0) {
				t.Fatal("inline sensor uploaded evidence", api.uploads)
			}
			if !bytes.Equal(api.media, capture.Media) {
				t.Fatal("uploaded media differs from source")
			}
			got, err := sensorrecord.ValidatePublication(api.requests[0])
			if err != nil || !bytes.Equal(got.SampleJSON, capture.Capture.SampleJSON) || receipts[0].CapturedUnixNS != capture.Capture.CapturedUnixNS {
				t.Fatal("publication restamped source", err)
			}
		})
	}
}

func TestHeldDuplicatesAreSkippedAndLostPublishAckIsNotRetried(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "duplicate", true: "lost-ack"}[lost], func(t *testing.T) {
			first, cfg := captureFixture(t, "gas-1", 1)
			second, _ := captureFixture(t, "gas-1", 2)
			var reads atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := reads.Add(1)
				if n == 1 {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				value := first
				if n >= 4 {
					value = second
				}
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer server.Close()
			cfg.SceneURL, cfg.ReadTokenFile, cfg.MaxSamples = server.URL, filepath.Join(t.TempDir(), "token"), 2
			if err := os.WriteFile(cfg.ReadTokenFile, []byte(strings.Repeat("t", 32)), 0600); err != nil {
				t.Fatal(err)
			}
			api := newAPI(t, cfg)
			api.lostAck = lost
			emitted := 0
			err := Run(context.Background(), cfg, api, func(liveapp.Receipt) error { emitted++; return nil })
			if lost {
				if !errors.Is(err, liveapp.ErrUncertain) || api.publishes != 1 || emitted != 0 || reads.Load() != 2 {
					t.Fatal(err, api.publishes, emitted, reads.Load())
				}
			} else if err != nil || api.publishes != 2 || emitted != 2 || reads.Load() != 4 {
				t.Fatal(err, api.publishes, emitted, reads.Load())
			}
		})
	}
}

func TestConfigLoadsLidarWithoutChangingLegacyWorkflow(t *testing.T) {
	_, cfg := captureFixture(t, "lidar-atlas", 1)
	cfg.SceneURL, cfg.ReadTokenFile = "http://127.0.0.1:8091", "/config/read-token"
	cfg.TTLMS, cfg.PollIntervalMS, cfg.RequestTimeoutMS = 0, 0, 0
	raw, _ := json.Marshal(cfg)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil || got.TTLMS != 3000 || got.PollIntervalMS != 1000 || got.RequestTimeoutMS != 1500 || got.SensorID != cfg.SensorID {
		t.Fatal(got, err)
	}
	if _, err = liveapp.LoadConfig(path); !errors.Is(err, liveapp.ErrConfig) {
		t.Fatal("legacy whole-scene workflow changed", err)
	}
}

func TestOptionalActualOfficeCaptureFiles(t *testing.T) {
	checkActualCaptureFiles(t, "WORLD_HAZARD_CAPTURE_DIR", []string{"camera-a", "camera-b", "lidar-atlas", "gas-1", "audio-1"})
}

func TestOptionalSharedOfficeCaptureFiles(t *testing.T) {
	checkActualCaptureFiles(t, "WORLD_SHARED_CAPTURE_DIR", []string{"camera-a", "camera-b", "people-front", "people-oblique", "lidar-atlas", "gas-1", "audio-1"})
}

func checkActualCaptureFiles(t *testing.T, environment string, sensors []string) {
	t.Helper()
	dir := os.Getenv(environment)
	if dir == "" {
		t.Skip("set " + environment + " for actual MuJoCo source outputs")
	}
	for _, sensor := range sensors {
		raw, err := os.ReadFile(filepath.Join(dir, sensor+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var capture Capture
		if err = json.Unmarshal(raw, &capture); err != nil {
			t.Fatal(err)
		}
		s, err := capture.Capture.Sample()
		if err != nil {
			t.Fatal(err)
		}
		cfg := Config{RunID: s.RunID, Site: s.SiteID, Frame: s.Frame, SensorID: sensor, SimulationAnchorUnixNS: capture.Capture.SimulationAnchorUnixNS}
		got, err := Decode(raw, cfg)
		if err != nil || !bytes.Equal(got.Capture.SampleJSON, capture.Capture.SampleJSON) || !bytes.Equal(got.Media, capture.Media) {
			t.Fatal(sensor, err)
		}
		if _, err = sensorrecord.Publication(got.Capture, 3*time.Second); err != nil {
			t.Fatal(sensor, err)
		}
	}
}
