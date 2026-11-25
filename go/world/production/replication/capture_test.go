package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

func captureRequest(t *testing.T, kind string) (serviceorigin.Scope, serviceorigin.PublishRequest, sensorrecord.Capture) {
	t.Helper()
	native := kind == "sensorlink"
	if native {
		kind = "camera"
	}
	id := map[string]string{"gas": "gas-1", "lidar": "lidar-atlas", "camera": "camera-a", "audio": "audio-1"}[kind]
	sample := sensing.Sample{SchemaVersion: sensing.Schema, ID: "replication-test/" + id + "/17", SiteID: "office", Frame: world.FrameRef{ID: "floor", Revision: "one"}, SensorID: id, Kind: kind, Sequence: 17, SampleNS: int64(time.Second), RunID: "replication-test", ScenarioVersion: "mujoco-4-test-v1", Seed: 7, PackageSHA256: strings.Repeat("a", 64), SensorModel: "test-" + kind, PositionM: []float64{2, 1, 3}, UncertaintyM: .04, CalibrationRevision: "cal-v1"}
	switch kind {
	case "gas":
		sample.Gas = &sensing.Gas{ValuePPM: 73.25, UncertaintyPPM: 2.7, Species: "synthetic-test-gas"}
	case "lidar":
		sample.Lidar = &sensing.Lidar{PointsM: [][3]float64{{3, 1, 3}, {2, .2, 4}}, MaxRangeM: 6.1}
	case "camera":
		sample.Camera = &sensing.Camera{Width: 4, Height: 3, FX: 4, FY: 4, CX: 2, CY: 1.5, CameraToSite: []float64{1, 0, 0, 2, 0, 1, 0, 1, 0, 0, 1, 3, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}
		sample.Evidence = &sensing.EvidenceRef{SHA256: strings.Repeat("b", 64), ContentType: "image/png", Size: 1024}
	case "audio":
		sample.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(250 * time.Millisecond)}
		sample.Evidence = &sensing.EvidenceRef{SHA256: strings.Repeat("c", 64), ContentType: "audio/wav", Size: 4044}
	}
	// Preserve a noncompact native sample encoding through the signed transport.
	raw, err := json.MarshalIndent(sample, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	capture := sensorrecord.Capture{Version: 1, SceneBootID: "scene-test", CapturedUnixNS: time.Unix(1794400000, 0).UnixNano(), SimulationAnchorUnixNS: time.Unix(1767225600, 0).UnixNano(), SampleJSON: raw}
	if native {
		var jpegBytes bytes.Buffer
		if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 4, 3)), nil); err != nil {
			t.Fatal(err)
		}
		capture, err = sensorrecord.BindSensorLink(capture, sensorrecord.SensorLinkFrame{
			ChannelID: 4, Seq: 8, TsUs: uint64(capture.CapturedUnixNS / 1000), Flags: 1, Payload: jpegBytes.Bytes()})
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := sensorrecord.Publication(capture, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	scope := serviceorigin.Scope{Tenant: tenant, Site: "office", Device: deviceA, App: "com.example.sensor", Service: id, DeploymentGeneration: 3, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: sensorrecord.ComponentType, Stream: id}}}
	return scope, request, capture
}

type captureClock struct{ window trustedtime.Interval }

func (c *captureClock) Now() (trustedtime.Interval, error) { return c.window, nil }

func TestRealSignedTypedCaptureRepairPreservesBytesAndExpiredBounds(t *testing.T) {
	for _, kind := range []string{"gas", "lidar", "camera", "audio", "sensorlink"} {
		t.Run(kind, func(t *testing.T) {
			scope, request, capture := captureRequest(t, kind)
			captured := time.Unix(0, capture.CapturedUnixNS)
			now := captured.Add(100 * time.Millisecond)
			id := testidentity.NewWith(t, now, scope.Device)
			certs, err := wendycert.NewDevice(wendycert.Config{Tenant: tenant, Roots: [][]byte{id.RootDER}})
			if err != nil {
				t.Fatal(err)
			}
			clock := &captureClock{trustedtime.Interval{Earliest: now, Latest: now.Add(time.Millisecond), BootID: "capture-test", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-only-anchor"}}
			publisher, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: scope, Epoch: 2, Lifetime: time.Minute, Clock: clock, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = publisher.Close() })
			signed, err := publisher.Publish(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			verifier, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: tenant, Site: scope.Site, ExecutionDomain: scope.ExecutionDomain, Certificates: certs, Clock: clock, Authorize: func(_ context.Context, c serviceorigin.DelegationClaims, id wendycert.DeviceIdentity) error {
				if !reflect.DeepEqual(c.Scope, scope) || c.Epoch != 2 || id.Principal != scope.Device {
					return serviceorigin.ErrDenied
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			verified, err := verifier.Verify(context.Background(), signed)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(signed)
			component := verified.Component()
			item := Item{Entry: Entry{Kind: Record, Reference: proofref.Reference{Key: component.Key, Version: component.Version, EnvelopeSHA256: hash(raw), RecordSHA256: hash(signed.Record)}, ExpiresAtUnixNS: request.Timing.SampleTime.Add(request.TTL).UnixNano()}, Envelope: raw}
			until, err := RecordInterpretationUntil(verified.Record)
			if err != nil || !until.Equal(captured.Add(sensorrecord.MaxPhysicalAge)) {
				t.Fatal("original physical cap", until, err)
			}
			for _, delay := range []time.Duration{0, 7 * time.Second} {
				clock.window.Earliest, clock.window.Latest = now.Add(delay), now.Add(delay+time.Millisecond)
				clock.window.Monotonic = time.Second + delay
				f := setup(t, item)
				for _, service := range []*Service{f.a, f.b} {
					service.cfg.Authorize = func(context.Context, Identity, Direction) (Access, error) {
						return Access{1, []proofref.Source{item.Entry.Reference.Source()}}, nil
					}
				}
				f.b.cfg.RemoteAuthority = func(ctx context.Context, received Item) error {
					var proof serviceorigin.SignedRecord
					if err := json.Unmarshal(received.Envelope, &proof); err != nil {
						return err
					}
					_, err := verifier.Verify(ctx, proof)
					return err
				}
				result, err := f.b.SyncPeer(context.Background(), deviceA)
				if err != nil || !result.Complete || result.Admitted != 1 || !bytes.Equal(f.local.admitted[0].Envelope, raw) {
					t.Fatal("original transfer", delay, result, err)
				}
				gotUntil, err := RecordInterpretationUntil(verified.Record)
				if err != nil || !gotUntil.Equal(until) {
					t.Fatal("receipt renewed capture", gotUntil, err)
				}
				if delay > sensorrecord.MaxPhysicalAge && clock.window.Latest.Before(gotUntil) {
					t.Fatal("expired original became live")
				}
				if !reflect.DeepEqual(f.local.admitted[0].Entry, item.Entry) {
					t.Fatal("sample deadline or source version changed")
				}
			}
			got, err := sensorrecord.ValidatePublication(verified.Record.Publication)
			if err != nil || !reflect.DeepEqual(got, capture) || !bytes.Equal(verified.Record.Publication.Payload, request.Payload) {
				t.Fatal("native sample bytes changed", err)
			}
			media, err := got.Evidence()
			if err != nil || (media != nil) != (kind == "camera" || kind == "audio" || kind == "sensorlink") {
				t.Fatal("media reference changed", media, err)
			}
			if kind == "sensorlink" && (*media != capture.SensorLink.Evidence || media.ContentType != "image/jpeg") {
				t.Fatal("native JPEG reference replaced by original PNG")
			}
		})
	}
}

func TestCaptureTransportKeepsMediaSampleDeadlines(t *testing.T) {
	for _, kind := range []string{"camera", "audio", "sensorlink"} {
		t.Run(kind, func(t *testing.T) {
			scope, request, capture := captureRequest(t, kind)
			request.Timing.ErrorBound = 100 * time.Millisecond
			request.Timing.AgeHint = 150 * time.Millisecond
			if kind != "audio" {
				sample, _ := capture.Sample()
				sample.Camera.CalibrationValidForNS = int64(time.Second)
				capture.SampleJSON, _ = json.Marshal(sample)
				if capture.SensorLink != nil {
					capture.SensorLink.SampleSHA256 = hash(capture.SampleJSON)
					capture.SensorLink.CalibrationSHA256, _ = sensorrecord.CalibrationDigest(sample)
				}
				request.Payload, _ = sensorrecord.Encode(capture)
			}
			record := serviceorigin.Record{Scope: scope, Epoch: 2, Sequence: 1, Publication: request}
			sampleUntil, physicalUntil, err := sensorrecord.LiveDeadlines((serviceorigin.VerifiedRecord{Record: record}).Component())
			want := request.Timing.SampleTime.Add(850 * time.Millisecond)
			if kind == "audio" {
				want = request.Timing.SampleTime.Add(2600 * time.Millisecond)
			}
			got, validateErr := RecordInterpretationUntil(record)
			if err != nil || validateErr != nil || !sampleUntil.Equal(want) || !got.Equal(physicalUntil) ||
				!got.Equal(time.Unix(0, capture.CapturedUnixNS).Add(sensorrecord.MaxPhysicalAge)) {
				t.Fatal("original media deadlines changed", sampleUntil, physicalUntil, got, err, validateErr)
			}
		})
	}
}

func TestCaptureTransportRefusesInvalidBounds(t *testing.T) {
	for _, mode := range []string{"different-site", "physical-domain", "overflow", "consumed-ttl"} {
		t.Run(mode, func(t *testing.T) {
			scope, request, capture := captureRequest(t, "gas")
			switch mode {
			case "different-site":
				scope.Site = "other"
			case "physical-domain":
				scope.ExecutionDomain = "physical"
			case "overflow":
				capture.CapturedUnixNS = math.MaxInt64
				request.Payload, _ = sensorrecord.Encode(capture)
			case "consumed-ttl":
				request.Timing.AgeHint = request.TTL
			}
			if _, err := RecordInterpretationUntil(serviceorigin.Record{Scope: scope, Epoch: 2, Sequence: 1, Publication: request}); !errors.Is(err, ErrProtocol) {
				t.Fatal("invalid capture accepted", err)
			}
		})
	}
}

func TestCaptureTransportRefusesMalformedMediaMetadata(t *testing.T) {
	for _, kind := range []string{"camera", "audio", "sensorlink"} {
		for _, mode := range []string{"missing-reference", "wrong-content-type", "changed-binding", "consumed-interval"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				scope, request, capture := captureRequest(t, kind)
				sample, _ := capture.Sample()
				switch mode {
				case "missing-reference":
					sample.Evidence = nil
				case "wrong-content-type":
					sample.Evidence.ContentType = "application/octet-stream"
				case "changed-binding":
					if capture.SensorLink != nil {
						capture.SensorLink.TimestampUS++
					} else {
						sample.SampleNS++
					}
				case "consumed-interval":
					if sample.Audio != nil {
						request.Timing.AgeHint = request.TTL - time.Duration(sample.Audio.DurationNS)
					} else {
						sample.Camera.CalibrationValidForNS = int64(100 * time.Millisecond)
						request.Timing.AgeHint = 100 * time.Millisecond
					}
				}
				capture.SampleJSON, _ = json.Marshal(sample)
				request.Payload, _ = json.Marshal(capture) // Deliberately bypass the typed encoder.
				if _, err := RecordInterpretationUntil(serviceorigin.Record{Scope: scope, Epoch: 2, Sequence: 1, Publication: request}); !errors.Is(err, ErrProtocol) {
					t.Fatal("malformed media capture admitted", err)
				}
			})
		}
	}
}
