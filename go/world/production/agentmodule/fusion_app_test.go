package agentmodule

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/livefusion"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func fusionAppCapture(t *testing.T, sensor string, anchor int64) (sensorrecord.Capture, []byte) {
	t.Helper()
	kind := "camera"
	if sensor == "gas-1" {
		kind = "gas"
	}
	if sensor == "audio-1" {
		kind = "audio"
	}
	c, data := captureSchemaFixture(t, kind)
	s, err := c.Sample()
	if err != nil {
		t.Fatal(err)
	}
	s.SensorID = sensor
	s.ID = s.RunID + "/" + sensor + "/17"
	c.SimulationAnchorUnixNS = anchor
	if kind == "camera" {
		im := image.NewNRGBA(image.Rect(0, 0, 4, 3))
		for y := range 3 {
			for x := range 4 {
				im.Set(x, y, color.NRGBA{R: 240, G: 10, B: 10, A: 255})
			}
		}
		var b bytes.Buffer
		if e := png.Encode(&b, im); e != nil {
			t.Fatal(e)
		}
		data = b.Bytes()
		s.Evidence = captureSchemaReference(data, "image/png")
		captureSchemaSetSample(t, &c, s)
		if sensor == "camera-b" {
			b.Reset()
			if e := jpeg.Encode(&b, im, &jpeg.Options{Quality: 90}); e != nil {
				t.Fatal(e)
			}
			data = bytes.Clone(b.Bytes())
			c, err = sensorrecord.BindSensorLink(c, sensorrecord.SensorLinkFrame{ChannelID: 4, Seq: 9, TsUs: uint64(c.CapturedUnixNS / 1000), Flags: 1, Payload: data})
			if err != nil {
				t.Fatal(err)
			}
		}
	} else {
		if kind == "audio" {
			for i := range (len(data) - 44) / 2 {
				binary.LittleEndian.PutUint16(data[44+2*i:], uint16(int16(10000*math.Sin(2*math.Pi*880*float64(i)/8000))))
			}
			s.Evidence = captureSchemaReference(data, "audio/wav")
		}
		captureSchemaSetSample(t, &c, s)
	}
	if e := sensorrecord.ValidateMedia(c, data); e != nil {
		t.Fatal(e)
	}
	return c, data
}

// This exercises the ordinary app client against the real agent module. The
// Unix server uses Linux peer credentials and pidfds; only test commissioning
// and a generated device identity replace host provisioning. No World RPC is
// stubbed, and media passes through the owner-bound persistent evidence store.
func TestLinuxOrdinaryFusionThroughCommissionedModule(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const app = "com.example.multimodal"
	sensors := []string{"camera-a", "camera-b", "gas-1", "audio-1"}
	anchor := moduleTestNow.Add(-150 * time.Millisecond).UnixNano()
	var sources []livefusion.Source
	var selectors []serviceaccess.Source
	for _, id := range sensors {
		sources = append(sources, livefusion.Source{SensorID: id, App: app, Service: "source-" + id})
		selectors = append(selectors, serviceaccess.Source{App: app, Service: "source-" + id, ComponentType: sensorrecord.ComponentType, Stream: id})
	}
	resultSelector := serviceaccess.Source{App: app, Service: "fusion", ComponentType: resultrecord.ComponentType, Stream: "hazard"}
	f := setupModule(t, func(c *Config) {
		c.Policy.ExecutionDomain = "simulation"
		c.Policy.SampleClockDomain = "simulation:run-a"
		c.Policy.Grants = nil
		for _, source := range selectors {
			c.Policy.Grants = append(c.Policy.Grants, Grant{App: source.App, Service: source.Service, Publish: []serviceorigin.PublishScope{{ComponentType: source.ComponentType, Stream: source.Stream}}})
		}
		c.Policy.Grants = append(c.Policy.Grants, Grant{App: app, Service: "fusion", Publish: []serviceorigin.PublishScope{{ComponentType: resultrecord.ComponentType, Stream: "hazard"}}, Subscribe: selectors}, Grant{App: app, Service: "audit", Subscribe: []serviceaccess.Source{resultSelector}})
		c.EvidenceDirectory = filepath.Join(filepath.Dir(c.JournalPath), "evidence")
		c.ProofArchivePath = filepath.Join(filepath.Dir(c.JournalPath), "archive", "proofs.db")
		c.CreateProofArchive = true
		c.Limits = appservice.Limits{MessagesPerSecond: 200, MessageBurst: 400, BytesPerSecond: 8 << 20, ByteBurst: 16 << 20}
		c.CaptureSource = &CaptureSource{RunID: "run-a", BootID: "boot-original", PackageSHA256: string(bytes.Repeat([]byte("a"), 64)), AnchorUnixNS: anchor, Frame: c.Policy.Frames[0]}
	})
	original := map[proofref.Reference][]byte{}
	mediaOriginal := map[string][]byte{}
	for i, id := range sensors {
		capture, data := fusionAppCapture(t, id, anchor)
		writer := moduleDial(t, f.mount(t, i, uint64(i+1)).SocketPath)
		publication, e := sensorrecord.Publication(capture, 3*time.Second)
		if e != nil {
			t.Fatal(e)
		}
		ref, e := capture.Evidence()
		if e != nil {
			t.Fatal(e)
		}
		if ref != nil {
			upload, e := writer.PutEvidence(ctx, &appv1.PutEvidenceRequest{ComponentType: publication.ComponentType, Stream: publication.Stream, Sha256: ref.SHA256, Size: ref.Size, Data: data, ContentType: ref.ContentType, RetainUntilUnixMs: moduleTestNow.Add(2 * time.Minute).UnixMilli()})
			if e != nil || upload == nil || !upload.Complete || upload.Received != ref.Size {
				t.Fatal("media not retained through exact service socket", id, e)
			}
			mediaOriginal[ref.SHA256] = bytes.Clone(data)
		}
		request, e := appv1.EncodePublish(publication)
		if e != nil {
			t.Fatal(e)
		}
		ack := modulePublish(t, writer, request)
		sourceRef := archiveReference(t, ack)
		original[sourceRef] = bytes.Clone(ack.SignedRecordJson)
	}
	g := f.config.Policy.Grants[4]
	mount, e := f.manager.EnsureAccess(ctx, g.App, g.Service, 7, g.Publish, g.Subscribe)
	if e != nil {
		t.Fatal(e)
	}
	client := moduleDial(t, mount.SocketPath)
	cfg := livefusion.Config{Version: 1, Tenant: f.config.Policy.Tenant, Site: f.config.Policy.Site, Device: f.config.Policy.Device, App: app, Service: "fusion", ExpectedGeneration: 7, RootsDER: f.config.Roots, RunID: "run-a", SceneBootID: "boot-original", PackageSHA256: f.config.CaptureSource.PackageSHA256, SimulationAnchorUnixNS: anchor, Frame: f.config.Policy.Frames[0], ScenarioVersion: "mujoco-4-live-v1", Seed: 7, Sources: sources, ResultStream: "hazard", DurationMS: 10000, MaxAttempts: 1, RetainForMS: 60000, RequestTimeoutMS: 1000}
	result, e := livefusion.Run(ctx, cfg, client)
	if e != nil || result.Status != "complete" || !result.LocallyVerified || result.Evaluation == nil || result.Evaluation.State != fusion.HazardDetected {
		t.Fatalf("ordinary fusion failed through actual module: %s %s %v", result.Status, result.ErrorCode, e)
	}
	if len(result.Inputs) != 4 || len(result.Media) != 3 {
		t.Fatal("fusion did not read all original proofs/media")
	}
	for _, input := range result.Inputs {
		if !bytes.Equal(original[input.Reference], input.EnvelopeJSON) {
			t.Fatal("fusion replaced original source proof")
		}
	}
	for _, media := range result.Media {
		if !bytes.Equal(mediaOriginal[media.Reference.SHA256], media.Data) {
			t.Fatal("fusion replaced evidence bytes")
		}
	}
	var signed serviceorigin.SignedRecord
	if json.Unmarshal(result.SignedRecordJSON, &signed) != nil {
		t.Fatal("invalid acknowledged result")
	}
	verified, e := f.manager.verifier.Verify(ctx, signed)
	if e != nil {
		t.Fatal("result not signed by commissioned actual publisher", e)
	}
	derived, e := resultrecord.Decode(verified.Record.Publication.Payload)
	if e != nil || len(derived.Inputs) != 4 {
		t.Fatal("result omitted exact raw dependencies", e)
	}
	for _, ref := range derived.Inputs {
		if len(original[ref]) == 0 {
			t.Fatal("result rebound input authority")
		}
	}
	ref := exactReference(result.SignedRecordJSON, verified)
	proof, e := f.manager.archive.Get(ctx, ref.Source(), ref)
	if e != nil || !bytes.Equal(proof.Envelope, result.SignedRecordJSON) {
		t.Fatal("result acknowledgment preceded exact durable archive proof", e)
	}
	audit := f.config.Policy.Grants[5]
	auditMount, e := f.manager.EnsureAccess(ctx, audit.App, audit.Service, 8, nil, audit.Subscribe)
	if e != nil {
		t.Fatal(e)
	}
	auditClient := moduleDial(t, auditMount.SocketPath)
	if page := accessQuery(t, auditClient, []serviceaccess.Source{resultSelector}, 8, ""); len(page.SignedRecordJson) != 1 || !bytes.Equal(page.SignedRecordJson[0], result.SignedRecordJSON) {
		t.Fatal("current result not served as original acknowledged proof")
	}
	f.clock.mu.Lock()
	f.clock.window.Earliest = f.clock.window.Earliest.Add(6 * time.Second)
	f.clock.window.Latest = f.clock.window.Latest.Add(6 * time.Second)
	f.clock.window.Monotonic += 6 * time.Second
	f.clock.mu.Unlock()
	if page := accessQuery(t, auditClient, []serviceaccess.Source{resultSelector}, 8, ""); len(page.SignedRecordJson) != 0 {
		t.Fatal("expired physical capture kept result live")
	}
	for sourceRef, raw := range original {
		readProof(t, client, sourceRef, raw, derived.RetainUntilUnixMS)
	}
	readProof(t, auditClient, ref, result.SignedRecordJSON, derived.RetainUntilUnixMS)
}
