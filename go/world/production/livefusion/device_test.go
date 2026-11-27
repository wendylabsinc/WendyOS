package livefusion

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

const remoteFusionDevice = "spiffe://wendy.sh/tenant/" + testidentity.Tenant + "/device/33333333-3333-4333-8333-333333333333"

func TestSourceDeviceDefaultsAndExactServiceIdentity(t *testing.T) {
	c := lidarConfigFixture()
	c.Sources[2].Device = remoteFusionDevice
	// Matching app/service names on distinct devices are distinct publishers.
	c.Sources[2].Service = c.Sources[0].Service
	got, err := c.normalized()
	if err != nil {
		t.Fatal(err)
	}
	for i, source := range got.Sources {
		want := c.Device
		if source.SensorID == "gas-1" {
			want = remoteFusionDevice
		}
		if source.Device != want || selector(source.selector()).Device != want {
			t.Fatal("device lost during configuration or selector encoding", source)
		}
		if i != 2 && c.Sources[i].Device != "" {
			t.Fatal("normalization mutated caller sources")
		}
	}
	c.Sources[2].Device = c.Device
	if got.Sources[2].Device != remoteFusionDevice {
		t.Fatal("normalized source aliases caller")
	}
	if _, err := c.normalized(); err == nil {
		t.Fatal("duplicate local device/app/service accepted")
	}
	remote := lidarConfigFixture()
	remote.Sources = append(remote.Sources, Source{SensorID: "lidar-remote", Device: remoteFusionDevice, App: remote.App, Service: remote.Service})
	if _, err := remote.normalized(); err != nil {
		t.Fatal("remote inline service confused with local result signer", err)
	}
}

func TestSourceDeviceRefusesForeignTenantAndDuplicateSensor(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"malformed": func(c *Config) { c.Sources[2].Device = "other-device" },
		"foreign tenant": func(c *Config) {
			c.Sources[2].Device = strings.Replace(remoteFusionDevice, testidentity.Tenant, "44444444-4444-4444-8444-444444444444", 1)
		},
		"duplicate sensor across devices": func(c *Config) {
			c.Sources = append(c.Sources, Source{SensorID: "gas-1", Device: remoteFusionDevice, App: c.App, Service: "remote-gas"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := lidarConfigFixture()
			change(&c)
			if _, err := c.normalized(); err == nil {
				t.Fatal("invalid device configuration accepted")
			}
		})
	}
}

func TestMixedDeviceCameraAudioFetchesExactOriginalOwner(t *testing.T) {
	for _, mode := range []string{"complete", "lidar", "chunked"} {
		t.Run(mode, func(t *testing.T) {
			s, api := setup(t, mode)
			// The chunked case also puts the large camera image on the peer.
			remote := []string{"camera-b", "audio-1"}
			if mode == "chunked" {
				remote = append(remote, "camera-a")
			}
			remoteFusionSources(t, s, remote...)
			out, err := Run(context.Background(), s.cfg, api)
			if err != nil || !out.LocallyVerified || s.published != 1 || len(out.Media) != 3 {
				t.Fatalf("remote media fusion: %s %s %v", out.Status, out.FailureStage, err)
			}
			_, result, err := decodeEnvelope(out.SignedRecordJSON)
			if err != nil || result.Scope.Device != s.cfg.Device {
				t.Fatal("result signer changed", err)
			}
			typed, err := fusioncapture.ValidateReplicaPublication(result.Publication)
			if err != nil {
				t.Fatal(err)
			}
			for i, input := range out.Inputs {
				source := input.Reference.Key.Source
				if !bytes.Equal(input.EnvelopeJSON, s.original[source.Stream]) || typed.Result.Inputs[i] != input.Reference || typed.Hazard.Inputs[i].Source != source {
					t.Fatal("remote original or derived lineage changed")
				}
			}
			for _, media := range out.Media {
				want := s.cfg.Device
				for _, sensor := range remote {
					if sensor == media.Source.Stream {
						want = remoteFusionDevice
					}
				}
				if media.Source.Device != want || s.owners[media.Source.Stream] == 0 || !bytes.Equal(media.Data, s.media[media.Source.Stream+"/"+media.Reference.SHA256]) {
					t.Fatal("evidence lost original owner or bytes")
				}
			}
			if mode == "chunked" && s.owners["camera-a"] < 2 {
				t.Fatal("large remote image did not use bounded ranges")
			}
		})
	}
}

// Re-sign unchanged capture publications with an independently generated device
// identity. The actual Unix-gRPC fixture returns those original envelopes.
// This tests the app, not native transport or a deployed Agent's admission.
func remoteFusionSources(t *testing.T, s *fixtureServer, ids ...string) {
	t.Helper()
	w, _ := s.clock.Now()
	id := testidentity.NewWith(t, w.Earliest, remoteFusionDevice)
	s.cfg.RootsDER = append(s.cfg.RootsDER, id.RootDER)
	certificates, err := wendycert.NewDevice(wendycert.Config{Tenant: s.cfg.Tenant, Roots: s.cfg.RootsDER})
	if err != nil {
		t.Fatal(err)
	}
	for _, sensor := range ids {
		for i := range s.cfg.Sources {
			source := &s.cfg.Sources[i]
			if source.SensorID != sensor {
				continue
			}
			_, original, err := decodeEnvelope(s.original[sensor])
			if err != nil {
				t.Fatal(err)
			}
			source.Device = remoteFusionDevice
			p, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{
				Scope: serviceorigin.Scope{Tenant: s.cfg.Tenant, Site: s.cfg.Site, Device: source.Device, App: source.App, Service: source.Service, DeploymentGeneration: 2, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: sensorrecord.ComponentType, Stream: sensor}}},
				Epoch: 29, Lifetime: 10 * time.Minute, Clock: s.clock, Certificates: certificates, ChainDER: id.ChainDER, DeviceKey: id.Key,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			s.publishers[sensor] = p
			signed, err := p.Publish(context.Background(), original.Publication)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(signed)
			if err != nil {
				t.Fatal(err)
			}
			s.proofs[sensor], s.original[sensor] = raw, bytes.Clone(raw)
		}
	}
}

func TestMixedDeviceFusionPreservesOriginalsOwnersAndDeadlines(t *testing.T) {
	for _, mode := range []string{"complete", "lidar"} {
		t.Run(mode, func(t *testing.T) {
			s, api := setup(t, mode)
			remoteFusionSources(t, s, "gas-1")
			// Give the remote gas the earliest physical and sample deadlines.
			s.proofs["gas-1"] = captureWithBounds(t, s, "gas-1", 250*time.Millisecond, time.Second, time.Second)
			s.original["gas-1"] = bytes.Clone(s.proofs["gas-1"])
			for i := range s.cfg.Sources {
				if s.cfg.Sources[i].SensorID != "gas-1" {
					s.cfg.Sources[i].Device = "" // Existing local configuration remains valid.
				}
			}
			out, err := Run(context.Background(), s.cfg, api)
			if err != nil || !out.LocallyVerified || s.published != 1 || len(out.Inputs) != len(s.cfg.Sources) || len(out.Media) != 3 {
				t.Fatalf("mixed device fusion: %s %s %v", out.Status, out.FailureStage, err)
			}
			_, result, err := decodeEnvelope(out.SignedRecordJSON)
			if err != nil || result.Scope.Device != s.cfg.Device {
				t.Fatal("result signer changed", err)
			}
			typed, err := fusioncapture.ValidateReplicaPublication(result.Publication)
			if err != nil {
				t.Fatal(err)
			}
			_, gas, _ := decodeEnvelope(s.original["gas-1"])
			component := (serviceorigin.VerifiedRecord{Record: gas}).Component()
			sampleUntil, physicalUntil, err := sensorrecord.LiveDeadlines(component)
			if err != nil || !typed.SampleValidUntil.Equal(sampleUntil) || !typed.PhysicalValidUntil.Equal(physicalUntil) {
				t.Fatal("remote capture deadlines changed", err, typed.SampleValidUntil, typed.PhysicalValidUntil)
			}
			for i, input := range out.Inputs {
				sensor := input.Reference.Key.Source.Stream
				want := s.cfg.Device
				if sensor == "gas-1" {
					want = remoteFusionDevice
				}
				if input.Reference.Key.Source.Device != want || !bytes.Equal(input.EnvelopeJSON, s.original[sensor]) {
					t.Fatal("input was reattributed or rewritten")
				}
				// Both the generic proof ancestry and interpreted world sources
				// preserve the original device, version and proof hash.
				if typed.Result.Inputs[i] != input.Reference || typed.Hazard.Inputs[i].Source != input.Reference.Key.Source || out.Evaluation.Hazard.Inputs[i].Source != input.Reference.Key.Source {
					t.Fatal("derived lineage lost an input device")
				}
			}
			for _, media := range out.Media {
				if media.Source.Device != s.cfg.Device || s.owners[media.Source.Stream] == 0 {
					t.Fatal("evidence owner lost local device")
				}
			}
			if s.owners["gas-1"] != 0 || s.queries != 5 {
				t.Fatal("remote inline input changed evidence reads or authority rounds")
			}
		})
	}
}

func TestMixedDeviceRemoteLidarNeedsNoMediaTransfer(t *testing.T) {
	s, api := setup(t, "lidar")
	remoteFusionSources(t, s, "gas-1", "lidar-a")
	s.omitted = map[string]bool{"lidar-b": true, "lidar-c": true}
	out, err := Run(context.Background(), s.cfg, api)
	if err != nil || !out.LocallyVerified || len(out.Inputs) != 5 || out.Evaluation.Hazard.Spatial == nil || !out.Evaluation.Hazard.Spatial.Associated {
		t.Fatal("remote inline scan could not support spatial fusion", out.Status, err)
	}
	for _, input := range out.Inputs {
		if (input.Reference.Key.Source.Stream == "gas-1" || input.Reference.Key.Source.Stream == "lidar-a") && input.Reference.Key.Source.Device != remoteFusionDevice {
			t.Fatal("remote inline source changed device")
		}
	}
	if s.owners["gas-1"] != 0 || s.owners["lidar-a"] != 0 || len(out.Media) != 3 {
		t.Fatal("remote inline source fetched media")
	}
}

func TestMixedDeviceSubstitutionNeverSatisfiesConfiguredSource(t *testing.T) {
	for _, boundary := range []int{1, 2} {
		t.Run(map[int]string{1: "discovery", 2: "authorization"}[boundary], func(t *testing.T) {
			s, api := setup(t, "complete")
			local := bytes.Clone(s.proofs["gas-1"])
			remoteFusionSources(t, s, "gas-1")
			client := &queryFaultClient{WorldAppServiceClient: api, fault: func(n int) error {
				if n == boundary {
					s.mu.Lock()
					s.proofs["gas-1"] = local
					s.mu.Unlock()
				}
				return nil
			}}
			out, err := Run(context.Background(), s.cfg, client)
			if err == nil || out.Status != "failed" || s.published != 0 || len(out.Media) != 0 || client.calls != boundary {
				t.Fatal("local proof substituted for remote same-name source", out.Status, err)
			}
		})
	}
}

func TestMixedDeviceExpiredOriginalWaitsForFreshRemoteCapture(t *testing.T) {
	for _, domain := range []string{"physical", "sample"} {
		t.Run(domain, func(t *testing.T) {
			s, api := setup(t, "complete")
			remoteFusionSources(t, s, "gas-1")
			age, ttl := time.Duration(0), 3*time.Second
			if domain == "physical" {
				age = 6 * time.Second
			} else {
				ttl = 100 * time.Millisecond
			}
			s.proofs["gas-1"] = captureWithBounds(t, s, "gas-1", 250*time.Millisecond, age, ttl)
			app, err := New(s.cfg, api)
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			old, err := app.Step(context.Background())
			if err != nil || old.Status != "insufficient_evidence" || s.published != 0 {
				t.Fatal("expired remote capture published or stopped acquisition", old.Status, err)
			}
			fresh := captureAt(t, s, "gas-1", 300*time.Millisecond)
			s.mu.Lock()
			s.proofs["gas-1"] = fresh
			s.mu.Unlock()
			next, err := app.Step(context.Background())
			if err != nil || !next.LocallyVerified || s.published != 1 {
				t.Fatal("fresh remote capture did not recover", next.Status, err)
			}
			for _, input := range next.Inputs {
				if input.Reference.Key.Source.Stream == "gas-1" && (!bytes.Equal(input.EnvelopeJSON, fresh) || input.Reference.Key.Source.Device != remoteFusionDevice) {
					t.Fatal("fresh remote original was replaced")
				}
			}
		})
	}
}
