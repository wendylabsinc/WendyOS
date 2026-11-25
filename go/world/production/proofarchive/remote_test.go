package proofarchive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type remoteFixture struct {
	e         *environment
	id        *testidentity.Fixture
	certs     *wendycert.DeviceVerifier
	verifier  *serviceorigin.Verifier
	accepted  map[string][]byte
	sampleNow time.Time
	deny      bool
	calls     int
	hook      func(int, []byte) error
}

func remoteSetup(t *testing.T) *remoteFixture {
	t.Helper()
	e := setup(t)
	r := &remoteFixture{e: e, accepted: map[string][]byte{}, sampleNow: time.Unix(1001, 0).UTC()}
	r.id = testidentity.NewWith(t, e.clock.w.Earliest, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/33333333-3333-4333-8333-333333333333")
	var err error
	r.certs, err = wendycert.NewDevice(wendycert.Config{Tenant: r.id.Tenant, Roots: [][]byte{r.id.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	r.verifier, err = serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: r.id.Tenant, Site: e.cfg.Site, ExecutionDomain: "simulation", Clock: e.clock, Certificates: r.certs, Authorize: func(_ context.Context, c serviceorigin.DelegationClaims, _ wendycert.DeviceIdentity) error {
		if r.deny || c.Scope.Device != r.id.Principal {
			return ErrDenied
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.RemoteVerifier, e.cfg.ValidateRemoteAccepted = r.verifier, r.check
	e.cfg.ValidatePin = func(ctx context.Context, claim serviceorigin.VerifiedRecord, inputs []Descriptor, _ time.Time) error {
		var body struct {
			Inputs []Reference `json:"inputs"`
		}
		if json.Unmarshal(claim.Record.Publication.Payload, &body) != nil || len(body.Inputs) != len(inputs) {
			return ErrDenied
		}
		for i, d := range inputs {
			if d.Reference != body.Inputs[i] {
				return ErrDenied
			}
			_, ref, err := e.a.verifyFor(ctx, d.Envelope, d.Reference.Key.Source.Device != e.cfg.Device)
			if err != nil || ref != d.Reference {
				return ErrDenied
			}
		}
		return nil
	}
	if err := e.a.Close(); err != nil {
		t.Fatal(err)
	}
	e.a, err = Open(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Models the host's already-held operation: exact admitted bytes plus fresh
// original sample/physical bounds. It never calls the archive or a journal.
func (r *remoteFixture) check(ctx context.Context, v serviceorigin.VerifiedRecord, raw []byte) error {
	r.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.deny || !bytes.Equal(raw, r.accepted[digest(raw)]) || v.Record.Scope.Device != r.id.Principal {
		return ErrDenied
	}
	capture, err := sensorrecord.ValidatePublication(v.Record.Publication)
	if err != nil {
		return ErrDenied
	}
	w, _ := r.e.clock.Now()
	sampleUntil, physicalUntil, err := sensorrecord.LiveDeadlines(v.Component())
	if err != nil || sensorrecord.PhysicalCurrent(capture, w) != nil ||
		!r.sampleNow.Before(sampleUntil) || !w.Latest.Before(physicalUntil) {
		return ErrUnavailable
	}
	if r.hook != nil {
		return r.hook(r.calls, raw)
	}
	return nil
}

func archiveSigned(t *testing.T, e *environment, id *testidentity.Fixture, certs *wendycert.DeviceVerifier, site string, p serviceorigin.PublishRequest) []byte {
	t.Helper()
	publisher, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: serviceorigin.Scope{Tenant: id.Tenant, Site: site, Device: id.Principal, App: "com.example.sensor", Service: "source", DeploymentGeneration: 1, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: p.ComponentType, Stream: p.Stream}}}, Epoch: 7, Lifetime: time.Hour, Clock: e.clock, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	signed, err := publisher.Publish(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(signed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func remoteRequest(t *testing.T, r *remoteFixture, kind string) serviceorigin.PublishRequest {
	t.Helper()
	native := kind == "sensorlink"
	if native {
		kind = "camera"
	}
	id := "gas-1"
	s := sensing.Sample{SchemaVersion: sensing.Schema, SiteID: r.e.cfg.Site, Frame: world.FrameRef{ID: "floor", Revision: "r1"}, Kind: kind, Sequence: 1, SampleNS: int64(time.Second), RunID: "run", ScenarioVersion: "test", Seed: 1, PackageSHA256: strings.Repeat("a", 64), SensorModel: "fixture", PositionM: []float64{0, 0, 0}, UncertaintyM: .1, CalibrationRevision: "cal-1"}
	switch kind {
	case "gas":
		s.Gas = &sensing.Gas{ValuePPM: 80, UncertaintyPPM: 2, Species: "synthetic"}
	case "lidar":
		id, s.Lidar = "lidar-atlas", &sensing.Lidar{PointsM: [][3]float64{{1, 0, 2}}, MaxRangeM: 6.1}
	case "camera":
		id, s.Evidence = "camera-a", &sensing.EvidenceRef{SHA256: strings.Repeat("b", 64), Size: 32, ContentType: "image/png"}
		s.Camera = &sensing.Camera{Width: 8, Height: 6, FX: 8, FY: 8, CX: 4, CY: 3, CameraToSite: []float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Minute)}
	case "audio":
		id, s.Evidence = "audio-1", &sensing.EvidenceRef{SHA256: strings.Repeat("c", 64), Size: 4044, ContentType: "audio/wav"}
		s.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(250 * time.Millisecond)}
	}
	s.SensorID, s.ID = id, "run/"+id+"/1"
	capture := sensorrecord.Capture{Version: 1, SceneBootID: "scene-boot", CapturedUnixNS: r.e.clock.w.Earliest.UnixNano(), SimulationAnchorUnixNS: time.Unix(1000, 0).UnixNano(), SampleJSON: encode(s)}
	if native {
		var jpegBytes bytes.Buffer
		if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 8, 6)), nil); err != nil {
			t.Fatal(err)
		}
		var err error
		capture, err = sensorrecord.BindSensorLink(capture, sensorrecord.SensorLinkFrame{
			ChannelID: 4, Seq: 2, TsUs: uint64(capture.CapturedUnixNS / 1000), Flags: 1, Payload: jpegBytes.Bytes()})
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := sensorrecord.Publication(capture, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (r *remoteFixture) raw(t *testing.T, kind string) []byte {
	t.Helper()
	raw := archiveSigned(t, r.e, r.id, r.certs, r.e.cfg.Site, remoteRequest(t, r, kind))
	r.accepted[digest(raw)] = bytes.Clone(raw)
	return raw
}

func TestRemoteMixedOriginsPinExactBytesAndReopen(t *testing.T) {
	r := remoteSetup(t)
	e := r.e
	localRaw := archiveSigned(t, e, e.identity, e.certificates, e.cfg.Site, remoteRequest(t, r, "gas"))
	local := remember(t, e, localRaw)
	gasRaw, lidarRaw := r.raw(t, "gas"), r.raw(t, "lidar")
	gas, err := e.a.RememberRemoteAccepted(context.Background(), gasRaw)
	if err != nil {
		t.Fatal(err)
	}
	lidar, err := e.a.RememberRemoteAccepted(context.Background(), lidarRaw)
	if err != nil {
		t.Fatal(err)
	}
	if local.Key.Entity != gas.Key.Entity || local.Key.Source.Device == gas.Key.Source.Device {
		t.Fatal("fixture must distinguish same-named local and remote captures")
	}
	before := get(t, e, gas)
	e.clock.advance(time.Second)
	r.hook = func(_ int, raw []byte) error { clear(raw); return nil }
	if duplicate, err := e.a.RememberRemoteAccepted(context.Background(), gasRaw); err != nil || duplicate != gas {
		t.Fatalf("duplicate: %v", err)
	}
	r.hook = nil
	if after := get(t, e, gas); !reflect.DeepEqual(before, after) {
		t.Fatal("duplicate or callback mutation changed original bytes/admission/deadlines")
	}
	inputs := []Reference{local, gas, lidar}
	claimRaw := e.raw(t, true, "mixed", inputs...)
	until := e.until(2 * time.Hour)
	pin, err := e.a.Pin(context.Background(), PinRequest{ClaimEnvelope: claimRaw, Inputs: inputs, RetainUntilUnixMS: until})
	if err != nil {
		t.Fatal(err)
	}
	claim := remember(t, e, claimRaw)
	if claim != pin.Claim {
		t.Fatal("claim identity changed")
	}
	wrongOwner := gas.Source()
	wrongOwner.Device = e.cfg.Device
	if _, err := e.a.Get(context.Background(), wrongOwner, gas); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("local alias selected a remote proof: %v", err)
	}
	// Current authority/certificates expire, while explicitly pinned audit bytes
	// remain historical. Neither Open nor Get calls the current remote callback.
	e.clock.advance(65 * time.Minute)
	r.deny = true
	calls := r.calls
	if err := e.a.Close(); err != nil {
		t.Fatal(err)
	}
	localOnly := e.cfg
	localOnly.RemoteVerifier, localOnly.ValidateRemoteAccepted = nil, nil
	if opened, err := Open(localOnly); !errors.Is(err, ErrCorrupt) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("remote history opened without explicit host capability: %v", err)
	}
	e.a, err = Open(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i, ref := range []Reference{local, gas, lidar, claim} {
		proof := get(t, e, ref)
		if !proof.Historical || proof.RetainUntilUnixMS != until || !bytes.Equal(proof.Envelope, [][]byte{localRaw, gasRaw, lidarRaw, claimRaw}[i]) {
			t.Fatal("historical proof bytes/owner/retention changed")
		}
	}
	if got, err := e.a.CheckPin(context.Background(), claim); err != nil || !reflect.DeepEqual(got, pin) {
		t.Fatalf("pin reopen: %v", err)
	}
	if r.calls != calls {
		t.Fatal("historical read invoked live admission")
	}
}

func TestRemoteAdmissionRequiresExplicitHostAndExactScope(t *testing.T) {
	r := remoteSetup(t)
	e := r.e
	good := r.raw(t, "gas")
	if _, err := e.a.RememberAccepted(context.Background(), good); !errors.Is(err, ErrDenied) {
		t.Fatalf("local API accepted foreign proof: %v", err)
	}
	if _, err := e.a.RememberRemoteAccepted(context.Background(), e.raw(t, false, "local")); !errors.Is(err, ErrDenied) {
		t.Fatalf("remote API accepted local proof: %v", err)
	}
	mismatchedSite := remoteRequest(t, r, "gas")
	capture, err := sensorrecord.ValidatePublication(mismatchedSite)
	if err != nil {
		t.Fatal(err)
	}
	sample, err := capture.Sample()
	if err != nil {
		t.Fatal(err)
	}
	sample.SiteID = "other-site"
	capture.SampleJSON = encode(sample)
	mismatchedSite.Payload, err = sensorrecord.Encode(capture)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"wrong site": archiveSigned(t, e, r.id, r.certs, "other-site", remoteRequest(t, r, "gas")),
		"inner site": archiveSigned(t, e, r.id, r.certs, e.cfg.Site, mismatchedSite),
	} {
		t.Run(name, func(t *testing.T) {
			before := r.calls
			if _, err := e.a.RememberRemoteAccepted(context.Background(), raw); !errors.Is(err, ErrDenied) {
				t.Fatalf("accepted: %v", err)
			}
			if r.calls != before {
				t.Fatal("invalid immutable scope reached host callback")
			}
		})
	}
	// No remote authority callback or verifier can turn a malformed foreign
	// tenant projection into an archive key, even before signature validation.
	var signed serviceorigin.SignedRecord
	_ = json.Unmarshal(good, &signed)
	var record serviceorigin.Record
	var delegation serviceorigin.DelegationClaims
	_ = json.Unmarshal(signed.Record, &record)
	_ = json.Unmarshal(signed.Delegation.Claims, &delegation)
	record.Scope.Tenant, delegation.Scope.Tenant = "44444444-4444-4444-8444-444444444444", "44444444-4444-4444-8444-444444444444"
	record.Scope.Device = strings.ReplaceAll(record.Scope.Device, r.id.Tenant, record.Scope.Tenant)
	delegation.Scope.Device = record.Scope.Device
	signed.Delegation.Claims = encode(delegation)
	record.DelegationSHA256 = digest(signed.Delegation.Claims)
	signed.Record = encode(record)
	if _, err := e.a.RememberRemoteAccepted(context.Background(), encode(signed)); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong tenant: %v", err)
	}
	_ = json.Unmarshal(good, &signed)
	signed.Signature[0] ^= 1
	before := r.calls
	if _, err := e.a.RememberRemoteAccepted(context.Background(), encode(signed)); err == nil {
		t.Fatal("invalid original signature accepted")
	}
	if r.calls != before {
		t.Fatal("invalid signature reached admission callback")
	}
	delete(r.accepted, digest(good))
	if _, err := e.a.RememberRemoteAccepted(context.Background(), good); !errors.Is(err, ErrDenied) {
		t.Fatalf("unadmitted exact bytes: %v", err)
	}
	_, ref, err := e.a.decodeEnvelope(good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Get(context.Background(), ref.Source(), ref); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("rejected proof retained: %v", err)
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.RemoteVerifier = nil }, func(c *Config) { c.ValidateRemoteAccepted = nil }} {
		cfg := e.cfg
		mutate(&cfg)
		if _, err := normalize(cfg); !errors.Is(err, ErrDenied) {
			t.Fatalf("partial configuration: %v", err)
		}
	}
	cfg := e.cfg
	cfg.RemoteVerifier, cfg.ValidateRemoteAccepted = nil, nil
	cfg, err = normalize(cfg)
	if err != nil {
		t.Fatalf("local default: %v", err)
	}
	localOnly := newArchive(cfg, e.a.db)
	if _, _, err := localOnly.decodeEnvelope(good); !errors.Is(err, ErrDenied) {
		t.Fatalf("remote opt-in absent: %v", err)
	}
}

func TestRemoteResultsAndForeignClaimsCannotBecomePinInputs(t *testing.T) {
	r := remoteSetup(t)
	e := r.e
	p := remoteRequest(t, r, "gas")
	p.ComponentType, p.Stream, p.Payload = resultrecord.ComponentType, "results", []byte(`{"version":1}`)
	remoteResult := archiveSigned(t, e, r.id, r.certs, e.cfg.Site, p)
	if _, err := e.a.RememberRemoteAccepted(context.Background(), remoteResult); !errors.Is(err, ErrDenied) {
		t.Fatalf("remote derived import: %v", err)
	}
	localResult := archiveSigned(t, e, e.identity, e.certificates, e.cfg.Site, p)
	resultRef := remember(t, e, localResult)
	claim := e.raw(t, true, "recursive", resultRef)
	if _, err := e.a.Pin(context.Background(), PinRequest{ClaimEnvelope: claim, Inputs: []Reference{resultRef}, RetainUntilUnixMS: e.until(time.Minute)}); !errors.Is(err, ErrDenied) {
		t.Fatalf("recursive result input: %v", err)
	}
	remote := r.raw(t, "gas")
	ref, err := e.a.RememberRemoteAccepted(context.Background(), remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Pin(context.Background(), PinRequest{ClaimEnvelope: remote, Inputs: []Reference{resultRef}, RetainUntilUnixMS: e.until(time.Minute)}); !errors.Is(err, ErrDenied) {
		t.Fatalf("foreign claim: %v", err)
	}
	if _, err := e.a.CheckPin(context.Background(), ref); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("foreign pin check: %v", err)
	}
	p = remoteRequest(t, r, "gas")
	p.Tombstone, p.Payload = true, nil
	tombstone := archiveSigned(t, e, r.id, r.certs, e.cfg.Site, p)
	if _, err := e.a.RememberRemoteAccepted(context.Background(), tombstone); !errors.Is(err, ErrDenied) {
		t.Fatalf("remote tombstone: %v", err)
	}
}

func TestRemoteCallbackRefusalAtCommitBoundaries(t *testing.T) {
	for _, boundary := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(boundary), func(t *testing.T) {
			r := remoteSetup(t)
			raw := r.raw(t, "camera")
			r.hook = func(n int, _ []byte) error {
				if n == boundary {
					return ErrDenied
				}
				return nil
			}
			ref, err := r.e.a.RememberRemoteAccepted(context.Background(), raw)
			if ref != (Reference{}) || !errors.Is(err, ErrDenied) || errors.Is(err, ErrUncertain) != (boundary == 3) {
				t.Fatalf("boundary result: %v %+v", err, ref)
			}
			if err := r.e.a.Close(); err != nil {
				t.Fatal(err)
			}
			r.e.a, err = Open(r.e.cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, ref, err = r.e.a.decodeEnvelope(raw)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := r.e.a.Get(context.Background(), ref.Source(), ref)
			if boundary == 3 {
				if err != nil || !bytes.Equal(proof.Envelope, raw) {
					t.Fatalf("committed exact proof lost: %v", err)
				}
			} else if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("precommit refusal stored a proof: %v", err)
			}
		})
	}
}

func TestRemoteRetentionNeverExtendsOriginalFreshness(t *testing.T) {
	for _, which := range []string{"sample", "physical", "authority"} {
		t.Run(which, func(t *testing.T) {
			r := remoteSetup(t)
			raw := r.raw(t, "lidar")
			ref, err := r.e.a.RememberRemoteAccepted(context.Background(), raw)
			if err != nil {
				t.Fatal(err)
			}
			switch which {
			case "sample":
				r.sampleNow = r.sampleNow.Add(3 * time.Second)
			case "physical":
				r.e.clock.advance(6 * time.Second)
			case "authority":
				r.deny = true
			}
			claim := r.e.raw(t, true, "expired-support", ref)
			if _, err := r.e.a.Pin(context.Background(), PinRequest{ClaimEnvelope: claim, Inputs: []Reference{ref}, RetainUntilUnixMS: r.e.until(time.Minute)}); err == nil {
				t.Fatal("old remote input gained live validity from retention")
			}
			if got := get(t, r.e, ref); !bytes.Equal(got.Envelope, raw) {
				t.Fatal("historical original changed")
			}
		})
	}
}
