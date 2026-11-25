package replicaprobe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appclock"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type Probe struct {
	mu              sync.Mutex
	cfg             Config
	client          appv1.WorldAppServiceClient
	clock           *appclock.Provider
	next            uint32
	epoch, sequence uint64
	stopped         bool
}

func New(c Config, client appv1.WorldAppServiceClient) (*Probe, error) {
	c, e := c.normalized()
	if e != nil || client == nil {
		return nil, ErrConfig
	}
	clock, e := appclock.New(client, c.clockConfig())
	if e != nil {
		return nil, ErrConfig
	}
	return &Probe{cfg: c, client: client, clock: clock, next: 1}, nil
}
func (p *Probe) Close() error {
	if p == nil {
		return nil
	}
	return p.clock.Close()
}
func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func code(err error) string {
	switch {
	case errors.Is(err, ErrUncertain):
		return "publication_outcome_unknown"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrCommand):
		return "command_refused"
	case errors.Is(err, ErrConfig):
		return "configuration_refused"
	case errors.Is(err, ErrResponse):
		return "response_refused"
	case errors.Is(err, ErrStopped):
		return "probe_stopped"
	case errors.Is(err, ErrAwait):
		return "query_expectation_unmet"
	default:
		return "socket_or_clock_refused"
	}
}

// Execute retries reads only for the explicit bounded await_query action. Any
// failed action stops this probe; a failed publication is always unknown outcome.
func (p *Probe) Execute(ctx context.Context, raw []byte) (out Receipt, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out = Receipt{Version: 1, RunID: p.cfg.RunID, Status: "failed", Originals: []Original{}, Validation: "socket_scope_and_structure_only"}
	defer func() {
		if err != nil {
			p.stopped = true
			out.ErrorCode = code(err)
			if errors.Is(err, ErrUncertain) {
				out.Status = "unknown_outcome"
			}
		}
	}()
	if p.stopped {
		return out, ErrStopped
	}
	if len(raw) > MaxCommandBytes {
		return out, ErrCommand
	}
	out.CommandJSON, out.CommandSHA256 = bytes.Clone(raw), digest(raw)
	command, e := DecodeCommand(raw)
	if e != nil || command.Sequence != p.next || int(command.Sequence) > p.cfg.MaxCommands {
		return out, ErrCommand
	}
	out.Sequence, out.Action = command.Sequence, command.Action
	if ctx == nil {
		return out, ErrCommand
	}
	if e = ctx.Err(); e != nil {
		return out, e
	}
	if command.Action == "finish" {
		p.stopped = true
		p.next++
		out.Status = "complete"
		return out, nil
	}
	if command.Action == "await_query" {
		if e = p.awaitQuery(ctx, command, &out); e != nil {
			return out, e
		}
		p.next++
		out.Status = "complete"
		return out, nil
	}
	observed, e := p.clock.Observe(ctx)
	if e != nil {
		return out, e
	}
	out.Clock = &ClockReceipt{observed.SampleTime.UnixNano(), observed.SampleClockDomain, observed.DeploymentGeneration, observed.ServerBootID, observed.Physical.Earliest.UnixNano(), observed.Physical.Latest.UnixNano(), int64(observed.RoundTrip)}
	call, cancel := context.WithTimeout(ctx, time.Duration(p.cfg.RequestTimeoutMS)*time.Millisecond)
	defer cancel()
	if command.Action == "query" {
		request := p.queryRequest()
		out.RequestJSON, _ = protojson.Marshal(request)
		reply, e := p.client.Query(call, request)
		out.RPCCode = status.Code(e).String()
		if e != nil {
			return out, e
		}
		out.ReplyJSON, out.Originals, e = p.queryReply(reply)
		if e != nil {
			return out, e
		}
	} else {
		request, e := p.request(command, observed)
		if e != nil {
			return out, e
		}
		out.RequestJSON, _ = protojson.Marshal(request)
		reply, e := p.client.Publish(call, request)
		out.RPCCode = status.Code(e).String()
		if e != nil {
			return out, errors.Join(ErrUncertain, e)
		}
		if reply != nil && proto.Size(reply) <= MaxEnvelopeBytes {
			out.ReplyJSON, _ = protojson.Marshal(reply)
		}
		if reply == nil || !reply.Accepted || len(reply.ProtoReflect().GetUnknown()) != 0 {
			return out, ErrUncertain
		}
		proof, record, e := p.original(reply.SignedRecordJson, p.cfg.Device)
		if e != nil {
			return out, errors.Join(ErrUncertain, e)
		}
		encoded, e := appv1.EncodePublish(record.Publication)
		if e != nil || !proto.Equal(encoded, request) || record.Scope.DeploymentGeneration != observed.DeploymentGeneration || p.epoch != 0 && (record.Epoch != p.epoch || record.Sequence <= p.sequence) {
			return out, ErrUncertain
		}
		p.epoch, p.sequence = record.Epoch, record.Sequence
		out.Originals = append(out.Originals, Original{bytes.Clone(reply.SignedRecordJson), digest(reply.SignedRecordJson), digest(proof.Record)})
	}
	p.next++
	out.Status = "complete"
	return out, nil
}

func (p *Probe) request(c Command, o appclock.Observation) (*appv1.PublishRequest, error) {
	r := serviceorigin.PublishRequest{Entity: Entity, ComponentType: Component, Stream: Stream, Frame: p.cfg.Frame, Timing: world.Timing{SampleTime: o.SampleTime, ClockDomain: o.SampleClockDomain, ErrorKnown: true}, TTL: time.Duration(c.TTLMS) * time.Millisecond, Tombstone: c.Action == "tombstone", Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "replica-probe-v1", Simulation: &world.SimulationContext{RunID: p.cfg.RunID, ScenarioVersion: p.cfg.ScenarioVersion, Seed: p.cfg.Seed, ClockDomain: p.cfg.SampleClockDomain}}}
	if !r.Tombstone {
		r.Payload, _ = json.Marshal(world.ScalarSample{SensorType: "synthetic-gas", Value: *c.Value, Unit: "ppm", CalibrationRevision: "replica-probe-v1", Uncertainty: 0.5})
	}
	return appv1.EncodePublish(r)
}

func (p *Probe) original(raw []byte, device string) (serviceorigin.SignedRecord, serviceorigin.Record, error) {
	var proof serviceorigin.SignedRecord
	var record serviceorigin.Record
	var delegation serviceorigin.DelegationClaims
	if len(raw) == 0 || len(raw) > MaxEnvelopeBytes || authority.DecodeStrictLimit(raw, &proof, MaxEnvelopeBytes) != nil || jws.DecodeStrict(proof.Record, &record) != nil || jws.DecodeStrict(proof.Delegation.Claims, &delegation) != nil {
		return proof, record, ErrResponse
	}
	canonical, e := json.Marshal(proof)
	if e != nil || !bytes.Equal(raw, canonical) || record.Version != serviceorigin.Version || record.AuthorityDomain != serviceorigin.AuthorityDomain || record.Scope.Validate() != nil || record.Scope.Tenant != p.cfg.Tenant || record.Scope.Site != p.cfg.Site || record.Scope.Device != device || record.Scope.App != App || record.Scope.Service != Service || record.Scope.ExecutionDomain != "simulation" || len(record.Scope.Publish) != 1 || record.Scope.Publish[0] != (serviceorigin.PublishScope{ComponentType: Component, Stream: Stream}) || record.Epoch == 0 || record.Sequence == 0 || record.DelegationSHA256 != digest(proof.Delegation.Claims) || record.IssuedAtUnixMS <= 0 || record.ExpiresAtUnixMS <= record.IssuedAtUnixMS || delegation.Version != serviceorigin.Version || delegation.AuthorityDomain != serviceorigin.AuthorityDomain || delegation.Epoch != record.Epoch || len(delegation.PublicKey) != 32 || len(proof.Signature) != 64 || len(proof.Delegation.Signature) != 3309 || len(proof.Delegation.ChainDER) == 0 || len(proof.Delegation.ChainDER) > 8 {
		return proof, record, ErrResponse
	}
	x, _ := json.Marshal(record.Scope)
	y, _ := json.Marshal(delegation.Scope)
	if !bytes.Equal(x, y) {
		return proof, record, ErrResponse
	}
	if delegation.IssuedAtUnixMS <= 0 || delegation.ExpiresAtUnixMS <= delegation.IssuedAtUnixMS || record.IssuedAtUnixMS < delegation.IssuedAtUnixMS || record.ExpiresAtUnixMS > delegation.ExpiresAtUnixMS {
		return proof, record, ErrResponse
	}
	chainBytes := 0
	for _, der := range proof.Delegation.ChainDER {
		chainBytes += len(der)
		if len(der) == 0 || chainBytes > 64<<10 {
			return proof, record, ErrResponse
		}
	}
	q := record.Publication
	sim := q.Provenance.Simulation
	if q.Entity != Entity || q.ComponentType != Component || q.Stream != Stream || q.Frame != p.cfg.Frame || q.Timing.ClockDomain != p.cfg.SampleClockDomain || !q.Timing.ErrorKnown || q.Provenance.Origin != world.Simulated || q.Provenance.AppVersion != "replica-probe-v1" || q.Provenance.ModelID != "" || q.Provenance.ModelVersion != "" || sim == nil || sim.RunID != p.cfg.RunID || sim.ScenarioVersion != p.cfg.ScenarioVersion || sim.Seed != p.cfg.Seed || sim.ClockDomain != p.cfg.SampleClockDomain || len(q.Provenance.Inputs) != 0 || q.Provenance.OriginalCaptureTime != nil || q.Provenance.OriginalClockDomain != "" || q.TTL < 100*time.Millisecond || q.TTL > 30*time.Second {
		return proof, record, ErrResponse
	}
	if _, e = appv1.EncodePublish(q); e != nil {
		return proof, record, ErrResponse
	}
	if q.Tombstone {
		if len(q.Payload) != 0 {
			return proof, record, ErrResponse
		}
	} else {
		var scalar world.ScalarSample
		if jws.DecodeStrict(q.Payload, &scalar) != nil || scalar.SensorType != "synthetic-gas" || scalar.Unit != "ppm" || scalar.CalibrationRevision != "replica-probe-v1" || scalar.Uncertainty != 0.5 || scalar.Value < 0 || scalar.Value > 1000000 {
			return proof, record, ErrResponse
		}
	}
	return proof, record, nil
}
