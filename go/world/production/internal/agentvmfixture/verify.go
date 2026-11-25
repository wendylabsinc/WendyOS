package agentvmfixture

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/agentmodule"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/authenticatedtime"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/encoding/protojson"
)

type ProofReceipt struct {
	Kind                     string `json:"kind"`
	Service                  string `json:"service"`
	Device                   string `json:"device"`
	Generation               uint64 `json:"deployment_generation"`
	Epoch                    uint64 `json:"epoch"`
	Sequence                 uint64 `json:"sequence"`
	ProofSHA256              string `json:"proof_sha256"`
	RecordSHA256             string `json:"record_sha256"`
	PayloadSHA256            string `json:"payload_sha256"`
	LeafSHA256               string `json:"leaf_sha256"`
	OriginalRequestPreserved bool   `json:"original_request_preserved"`
	ExecutionDomain          string `json:"execution_domain"`
	SampleClockDomain        string `json:"sample_clock_domain"`
	AuthorityTimeEvidence    string `json:"authority_time_evidence"`
}

// Verify uses real authenticated physical time. It establishes a publication
// proof under the exact test commission, not runtime app liveness or missions.
func Verify(ctx context.Context, directory, proof, request, service string, generation, epoch uint64) (ProofReceipt, error) {
	clock, err := authenticatedtime.Acquire(ctx, authenticatedtime.Config{Servers: authenticatedtime.WendyServers(), QueryTimeout: 3 * time.Second, MaxHoldover: 10 * time.Minute, DriftPPM: 100, MaxWidth: 5 * time.Second})
	if err != nil {
		return ProofReceipt{}, err
	}
	r, err := verify(ctx, directory, proof, request, service, generation, epoch, clock)
	if err == nil {
		r.AuthorityTimeEvidence = clock.Report().EvidenceID
	}
	return r, err
}

// The proof and original request are untrusted app output; bound reads and
// refuse nonregular files/symlinks, without requiring the app to be root.
func readOutput(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "public-fixture-output")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("regular public proof file required")
	}
	b, err := io.ReadAll(io.LimitReader(f, 256<<10+1))
	if err != nil || len(b) > 256<<10 {
		return nil, errors.New("public proof exceeds bound")
	}
	return b, nil
}
func verify(ctx context.Context, directory, proof, request, service string, generation, epoch uint64, clock trustedtime.Provider) (ProofReceipt, error) {
	if (service != "gas" && service != "temperature") || generation == 0 || epoch == 0 {
		return ProofReceipt{}, errors.New("explicit exact service, generation and epoch required")
	}
	p, err := readPublic(directory)
	if err != nil {
		return ProofReceipt{}, err
	}
	b, err := agentmodule.ReadCommissionedFile(receiptPath(directory, "issued.json"), 64<<10)
	var issued IssuedReceipt
	if err != nil || jws.DecodeStrict(b, &issued) != nil || issued.Kind != Kind || issued.Principal != p.Principal || issued.LeafSHA256 != digest(issued.LeafDER) || !issued.CSRVerified || !issued.EABVerified || !issued.AccountJWSVerified {
		return ProofReceipt{}, errors.New("complete public enrollment receipt required")
	}
	b, err = agentmodule.ReadCommissionedFile(filepath.Join(directory, "world.json"), 256<<10)
	var cfg agentmodule.FileConfig
	if err != nil || jws.DecodeStrict(b, &cfg) != nil || cfg.Policy.Tenant != p.Tenant || cfg.Policy.Device != p.Principal || cfg.Policy.Site != "office" || cfg.Policy.ExecutionDomain != "simulation" || cfg.Policy.SampleClockDomain != ClockDomain || len(cfg.RootsDER) != 1 || digest(cfg.RootsDER[0]) != p.RootSHA256 {
		return ProofReceipt{}, errors.New("original fixture commission required")
	}
	proofBytes, err := readOutput(proof)
	if err != nil {
		return ProofReceipt{}, err
	}
	var signed serviceorigin.SignedRecord
	if jws.DecodeStrict(proofBytes, &signed) != nil {
		return ProofReceipt{}, errors.New("invalid exact signed record")
	}
	requestBytes, err := readOutput(request)
	if err != nil {
		return ProofReceipt{}, err
	}
	var wire appv1.PublishRequest
	if protojson.Unmarshal(requestBytes, &wire) != nil {
		return ProofReceipt{}, errors.New("original request must be strict PublishRequest protojson")
	}
	original, err := appv1.DecodePublish(&wire)
	if err != nil {
		return ProofReceipt{}, err
	}
	expected := serviceorigin.Scope{Tenant: p.Tenant, Site: "office", Device: p.Principal, App: App, Service: service, DeploymentGeneration: generation, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: "wendy.sensor.scalar.v1", Stream: service}}}
	certs, err := wendycert.NewDevice(wendycert.Config{Tenant: p.Tenant, Roots: cfg.RootsDER})
	if err != nil {
		return ProofReceipt{}, err
	}
	v, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: p.Tenant, Site: "office", ExecutionDomain: "simulation", Certificates: certs, Clock: clock, Authorize: func(ctx context.Context, c serviceorigin.DelegationClaims, d wendycert.DeviceIdentity) error {
		w, e := clock.Now()
		if ctx.Err() != nil || e != nil || w.Earliest.Before(cfg.Policy.NotBefore) || !w.Latest.Before(cfg.Policy.NotAfter) || !reflect.DeepEqual(c.Scope, expected) || c.Epoch != epoch || d.CertificateSHA256 != issued.LeafSHA256 || d.SigningKeySHA256 != issued.DeviceSPKISHA256 {
			return serviceorigin.ErrDenied
		}
		for _, g := range cfg.Policy.Grants {
			if g.App == App && g.Service == service && reflect.DeepEqual(g.Publish, expected.Publish) {
				return nil
			}
		}
		return serviceorigin.ErrDenied
	}})
	if err != nil {
		return ProofReceipt{}, err
	}
	verified, err := v.Verify(ctx, signed)
	if err != nil {
		return ProofReceipt{}, err
	}
	if !reflect.DeepEqual(verified.Record.Publication, original) {
		return ProofReceipt{}, errors.New("signed publication differs from original request")
	}
	c := verified.Component()
	if c.Key.Type != "wendy.sensor.scalar.v1" || c.Key.Source.Stream != service || c.Frame != (world.FrameRef{ID: "office-floor", Revision: "vm-fixture-1"}) || c.Timing.ClockDomain != ClockDomain || !c.Timing.SampleTime.Equal(SampleTime) || c.Provenance.Origin != world.Simulated || c.Provenance.Simulation == nil || c.Provenance.Simulation.RunID != "agent-vm-fixture" || c.Provenance.Simulation.ScenarioVersion != "private-agent-v1" || c.Provenance.Simulation.Seed != 1 {
		return ProofReceipt{}, errors.New("fixture sample ancestry or frame mismatch")
	}
	var scalar world.ScalarSample
	if jws.DecodeStrict(c.Payload, &scalar) != nil {
		return ProofReceipt{}, errors.New("scalar schema mismatch")
	}
	o := world.Observation{ID: c.Key.Entity, Tenant: c.Key.Tenant, Site: c.Key.Site, Source: c.Key.Source, Frame: c.Frame, Timing: c.Timing, Provenance: c.Provenance, Scalar: &scalar}
	if o.Validate() != nil {
		return ProofReceipt{}, errors.New("invalid fixture scalar")
	}
	replica, err := world.NewReplica(world.Config{Tenant: p.Tenant, Site: "office", ClockDomain: ClockDomain, Now: func() time.Time { return SampleTime }, MaxEntries: 256, AllowSynthetic: true, Frames: cfg.Policy.Frames})
	if err != nil {
		return ProofReceipt{}, err
	}
	if _, err = replica.Apply(c.Key.Source, c); err != nil {
		return ProofReceipt{}, err
	}
	if _, live, err := replica.Get(c.Key); err != nil || !live {
		return ProofReceipt{}, errors.New("original sample is not live in commissioned simulation time")
	}
	return ProofReceipt{Kind: Kind, Service: service, Device: p.Principal, Generation: generation, Epoch: epoch, Sequence: verified.Record.Sequence, ProofSHA256: digest(proofBytes), RecordSHA256: digest(signed.Record), PayloadSHA256: digest(original.Payload), LeafSHA256: issued.LeafSHA256, OriginalRequestPreserved: true, ExecutionDomain: "simulation", SampleClockDomain: ClockDomain}, nil
}

// JSONReceipt encodes public evidence only. Callers must never serialize the
// private issuer state or enrollment handoff into their verification archive.
func JSONReceipt(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
