package liveapp

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
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type Publisher struct {
	mu          sync.Mutex
	client      appv1.WorldAppServiceClient
	cfg         PublisherConfig
	nextRPC     time.Time
	fault       error
	last        *Receipt
	lastRequest []byte
	run, sensor string
	anchor      int64
}

func NewPublisher(client appv1.WorldAppServiceClient, cfg PublisherConfig) (*Publisher, error) {
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 1500 * time.Millisecond
	}
	if cfg.MinRPCInterval == 0 {
		cfg.MinRPCInterval = 125 * time.Millisecond
	}
	if client == nil || cfg.RequestTimeout < 10*time.Millisecond || cfg.RequestTimeout > 5*time.Second || cfg.MinRPCInterval < 125*time.Millisecond || cfg.MinRPCInterval > time.Second {
		return nil, ErrConfig
	}
	return &Publisher{client: client, cfg: cfg}, nil
}
func hash(data []byte) string { v := sha256.Sum256(data); return hex.EncodeToString(v[:]) }
func (p *Publisher) pace(ctx context.Context) error {
	if wait := time.Until(p.nextRPC); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.nextRPC = time.Now().Add(p.cfg.MinRPCInterval)
	return nil
}
func (p *Publisher) uncertain(err error) error {
	// Do not preserve arbitrary server error text in public result JSON.
	p.fault = errors.Join(ErrUncertain, err)
	return p.fault
}

// Submit never retries a mutation. After any ambiguous RPC result, this
// publisher remains faulted so a caller cannot accidentally resubmit it.
func (p *Publisher) Submit(ctx context.Context, request serviceorigin.PublishRequest, media []byte) (Receipt, error) {
	if p == nil || ctx == nil {
		return Receipt{}, ErrConfig
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fault != nil {
		return Receipt{}, p.fault
	}
	capture, err := sensorrecord.ValidatePublication(request)
	if err != nil || sensorrecord.ValidateMedia(capture, media) != nil {
		return Receipt{}, ErrCapture
	}
	sample, err := capture.Sample()
	if err != nil {
		return Receipt{}, ErrCapture
	}
	wire, err := appv1.EncodePublish(request)
	if err != nil {
		return Receipt{}, ErrCapture
	}
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(wire)
	if err != nil {
		return Receipt{}, ErrCapture
	}
	if p.last != nil {
		if capture.SceneBootID != p.last.SceneBootID || sample.RunID != p.run || sample.SensorID != p.sensor || capture.SimulationAnchorUnixNS != p.anchor || sample.Sequence < p.last.NativeSequence || sample.SampleNS < p.last.SampleNS || capture.CapturedUnixNS < p.last.CapturedUnixNS {
			return Receipt{}, ErrCapture
		}
		if sample.Sequence == p.last.NativeSequence {
			if !bytes.Equal(raw, p.lastRequest) {
				return Receipt{}, ErrCapture
			}
			return Receipt{}, ErrDuplicate
		}
		if sample.SampleNS <= p.last.SampleNS {
			return Receipt{}, ErrCapture
		}
	}
	ref, err := capture.Evidence()
	if err != nil {
		return Receipt{}, ErrCapture
	}
	if ref != nil {
		var until int64
		for offset := 0; offset < len(media); {
			if err := p.pace(ctx); err != nil {
				return Receipt{}, err
			}
			end := min(offset+serviceevidence.MaxChunk, len(media))
			rpc, cancel := context.WithTimeout(ctx, p.cfg.RequestTimeout)
			ack, e := p.client.PutEvidence(rpc, &appv1.PutEvidenceRequest{ComponentType: request.ComponentType, Stream: request.Stream, Sha256: ref.SHA256, Size: ref.Size, Offset: int64(offset), Data: bytes.Clone(media[offset:end]), ContentType: ref.ContentType, RetainUntilUnixMs: until})
			cancel()
			if e != nil {
				return Receipt{}, p.uncertain(e)
			}
			if ack == nil || ack.Sha256 != ref.SHA256 || ack.Size != ref.Size || ack.ContentType != ref.ContentType || ack.RetainUntilUnixMs <= 0 || until != 0 && ack.RetainUntilUnixMs != until {
				return Receipt{}, p.uncertain(ErrAcknowledgment)
			}
			// A repeated digest may already be completely retained for this
			// owner. Its explicit complete acknowledgment covers the whole hash.
			// An unexplained partial offset ahead/behind is never resumed.
			if ack.Complete {
				if ack.Received != ref.Size {
					return Receipt{}, p.uncertain(ErrAcknowledgment)
				}
				end = len(media)
			} else if ack.Received != int64(end) || end == len(media) {
				return Receipt{}, p.uncertain(ErrAcknowledgment)
			}
			until = ack.RetainUntilUnixMs
			offset = end
		}
	}
	if err := p.pace(ctx); err != nil {
		return Receipt{}, err
	}
	rpc, cancel := context.WithTimeout(ctx, p.cfg.RequestTimeout)
	ack, err := p.client.Publish(rpc, wire)
	cancel()
	if err != nil {
		return Receipt{}, p.uncertain(err)
	}
	if ack == nil || !ack.Accepted {
		return Receipt{}, p.uncertain(ErrAcknowledgment)
	}
	record, err := acknowledged(ack.SignedRecordJson, wire)
	if err != nil {
		return Receipt{}, p.uncertain(err)
	}
	out := Receipt{CaptureID: sample.ID, NativeSequence: sample.Sequence, SceneBootID: capture.SceneBootID, CapturedUnixNS: capture.CapturedUnixNS, SampleNS: sample.SampleNS, RequestJSON: bytes.Clone(raw), SignedRecordJSON: bytes.Clone(ack.SignedRecordJson), RecordSHA256: hash(record), ProofSHA256: hash(ack.SignedRecordJson)}
	if ref != nil {
		out.EvidenceSHA256 = ref.SHA256
	}
	stored := out
	stored.RequestJSON = nil
	stored.SignedRecordJSON = nil
	p.last = &stored
	p.lastRequest = bytes.Clone(raw)
	p.run = sample.RunID
	p.sensor = sample.SensorID
	p.anchor = capture.SimulationAnchorUnixNS
	return out, nil
}

func acknowledged(raw []byte, request *appv1.PublishRequest) ([]byte, error) {
	var signed serviceorigin.SignedRecord
	if len(raw) > 256<<10 || authority.DecodeStrictLimit(raw, &signed, 256<<10) != nil || len(signed.Record) == 0 || len(signed.Record) > serviceorigin.MaxRecordBytes || len(signed.Signature) != 64 || len(signed.Delegation.Claims) == 0 || len(signed.Delegation.Claims) > serviceorigin.MaxClaimsBytes || len(signed.Delegation.ChainDER) == 0 || len(signed.Delegation.ChainDER) > 8 || len(signed.Delegation.Signature) == 0 {
		return nil, ErrAcknowledgment
	}
	var r serviceorigin.Record
	if jws.DecodeStrict(signed.Record, &r) != nil || r.Version != serviceorigin.Version || r.AuthorityDomain != serviceorigin.AuthorityDomain || r.Scope.Validate() != nil || r.Scope.ExecutionDomain != "simulation" || r.Epoch == 0 || r.Sequence == 0 || r.ExpiresAtUnixMS <= r.IssuedAtUnixMS {
		return nil, ErrAcknowledgment
	}
	encoded, err := appv1.EncodePublish(r.Publication)
	if err != nil || !proto.Equal(request, encoded) {
		return nil, ErrAcknowledgment
	}
	capture, err := sensorrecord.ValidatePublication(r.Publication)
	if err != nil {
		return nil, ErrAcknowledgment
	}
	sample, err := capture.Sample()
	if err != nil || r.Scope.Site != sample.SiteID {
		return nil, ErrAcknowledgment
	}
	allowed := false
	for _, scope := range r.Scope.Publish {
		if scope.ComponentType == request.ComponentType && scope.Stream == request.Stream {
			allowed = true
		}
	}
	if !allowed {
		return nil, ErrAcknowledgment
	}
	// This verifies acknowledgment content, not certificate or policy trust.
	var claims serviceorigin.DelegationClaims
	if jws.DecodeStrict(signed.Delegation.Claims, &claims) != nil || claims.Version != serviceorigin.Version || claims.AuthorityDomain != serviceorigin.AuthorityDomain || claims.Epoch != r.Epoch || r.DelegationSHA256 != hash(signed.Delegation.Claims) || claims.ExpiresAtUnixMS <= claims.IssuedAtUnixMS || r.IssuedAtUnixMS < claims.IssuedAtUnixMS || r.ExpiresAtUnixMS > claims.ExpiresAtUnixMS || len(claims.PublicKey) != 32 {
		return nil, ErrAcknowledgment
	}
	a, _ := json.Marshal(claims.Scope)
	b, _ := json.Marshal(r.Scope)
	if !bytes.Equal(a, b) {
		return nil, ErrAcknowledgment
	}
	return bytes.Clone(signed.Record), nil
}
