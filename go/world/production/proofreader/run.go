package proofreader

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appclock"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"google.golang.org/grpc"
)

type ProofReceipt struct {
	ExpectedProof
	RetainUntilUnixMS int64 `json:"retain_until_unix_ms"`
	Historical        bool  `json:"historical"`
}
type MediaReceipt struct {
	Source            serviceaccess.Source `json:"source"`
	Reference         sensing.EvidenceRef  `json:"reference"`
	Data              []byte               `json:"data"`
	RetainUntilUnixMS int64                `json:"retain_until_unix_ms"`
}
type Result struct {
	Version                     int            `json:"version"`
	Status                      string         `json:"status"`
	Error                       string         `json:"error,omitempty"`
	Proofs                      []ProofReceipt `json:"proofs,omitempty"`
	Media                       []MediaReceipt `json:"media,omitempty"`
	PhysicalCaptureExpired      bool           `json:"physical_capture_expired"`
	OriginalVersionsAbsent      bool           `json:"original_versions_absent"`
	CurrentSourceAuthority      bool           `json:"current_source_authority"`
	FinalPhysicalEarliestUnixNS int64          `json:"final_physical_earliest_unix_ns,omitempty"`
	FinalPhysicalLatestUnixNS   int64          `json:"final_physical_latest_unix_ns,omitempty"`
}

func source(ref proofref.Reference) serviceaccess.Source {
	return serviceaccess.Source{App: ref.Key.Source.App, Service: ref.Key.Source.Service, ComponentType: ref.Key.Type, Stream: ref.Key.Source.Stream}
}
func selector(s serviceaccess.Source) *appv1.SourceSelector {
	return &appv1.SourceSelector{App: s.App, Service: s.Service, ComponentType: s.ComponentType, Stream: s.Stream}
}
func Run(ctx context.Context, config Config, api appv1.WorldAppServiceClient) (out Result, err error) {
	out = Result{Version: 1, Status: "failed"}
	defer func() {
		if err != nil {
			out.Status = "failed"
			switch err {
			case ErrConfig:
				out.Error = "configuration_refused"
			case ErrProof:
				out.Error = "historical_proof_refused"
			case ErrMedia:
				out.Error = "historical_media_refused"
			case ErrLive:
				out.Error = "live_query_refused"
			default:
				out.Error = "clock_or_access_unavailable"
			}
		}
	}()
	cfg, err := prepare(config)
	if err != nil || api == nil || ctx == nil {
		return out, ErrConfig
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.cfg.DurationMS)*time.Millisecond)
	defer cancel()
	clock, err := appclock.New(api, appclock.Config{Tenant: config.Tenant, Site: config.Site, Device: config.Device, App: config.App, Service: config.Service, ExpectedGeneration: config.ExpectedGeneration, ExecutionDomain: "simulation", SampleClockDomain: "simulation:" + config.RunID, MaxRoundTrip: time.Duration(cfg.cfg.RequestTimeoutMS) * time.Millisecond, MaxWidth: time.Duration(cfg.cfg.ClockMaxWidthMS) * time.Millisecond, DriftPPM: 100})
	if err != nil {
		return out, err
	}
	defer clock.Close()
	for {
		observation, e := clock.Observe(ctx)
		if e != nil {
			return out, e
		}
		if observation.Physical.Earliest.After(cfg.expiredAfter) {
			break
		}
		timer := time.NewTimer(time.Duration(cfg.cfg.PollIntervalMS) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, ctx.Err()
		case <-timer.C:
		}
	}
	out.PhysicalCaptureExpired = true
	retained := []int64{}
	for _, row := range cfg.records {
		ref := row.expected.Reference
		rpc, stop := context.WithTimeout(ctx, time.Duration(cfg.cfg.RequestTimeoutMS)*time.Millisecond)
		reply, e := api.GetRecordProof(rpc, &appv1.GetRecordProofRequest{Source: selector(source(ref)), Entity: ref.Key.Entity, Epoch: ref.Version.Epoch, Sequence: ref.Version.Sequence, EnvelopeSha256: ref.EnvelopeSHA256, RecordSha256: ref.RecordSHA256}, grpc.MaxCallRecvMsgSize(MaxEnvelopeBytes+4096), grpc.MaxRetryRPCBufferSize(0))
		stop()
		if e != nil || reply == nil || len(reply.ProtoReflect().GetUnknown()) != 0 || !reply.Historical || reply.RetainUntilUnixMs <= 0 || !bytes.Equal(reply.SignedRecordJson, row.expected.EnvelopeJSON) {
			return out, ErrProof
		}
		_, got, e := decode(reply.SignedRecordJson)
		if e != nil || got != ref {
			return out, ErrProof
		}
		now, e := clock.Observe(ctx)
		if e != nil {
			return out, e
		}
		if !now.Physical.Latest.Before(time.UnixMilli(reply.RetainUntilUnixMs)) {
			return out, ErrProof
		}
		retained = append(retained, reply.RetainUntilUnixMs)
		out.Proofs = append(out.Proofs, ProofReceipt{ExpectedProof: ExpectedProof{Reference: ref, EnvelopeJSON: bytes.Clone(reply.SignedRecordJson)}, RetainUntilUnixMS: reply.RetainUntilUnixMs, Historical: true})
	}
	total := int64(0)
	for _, row := range cfg.records {
		if row.capture == nil {
			continue
		}
		ref, e := row.capture.Evidence()
		if e != nil {
			return out, ErrMedia
		}
		if ref == nil {
			continue
		}
		if ref.Size > (16<<20)-total {
			return out, ErrMedia
		}
		total += ref.Size
		data := make([]byte, 0, int(ref.Size))
		var until int64
		owner := source(row.expected.Reference)
		for int64(len(data)) < ref.Size {
			length := min(int64(serviceevidence.MaxChunk), ref.Size-int64(len(data)))
			rpc, stop := context.WithTimeout(ctx, time.Duration(cfg.cfg.RequestTimeoutMS)*time.Millisecond)
			reply, e := api.GetEvidence(rpc, &appv1.GetEvidenceRequest{Source: selector(owner), Sha256: ref.SHA256, Offset: int64(len(data)), Length: int32(length)}, grpc.MaxCallRecvMsgSize(serviceevidence.MaxChunk+4096), grpc.MaxRetryRPCBufferSize(0))
			stop()
			if e != nil || reply == nil || len(reply.ProtoReflect().GetUnknown()) != 0 || reply.Sha256 != ref.SHA256 || reply.Size != ref.Size || reply.Offset != int64(len(data)) || reply.ContentType != ref.ContentType || int64(len(reply.Data)) != length || reply.RetainUntilUnixMs <= 0 || until != 0 && until != reply.RetainUntilUnixMs {
				return out, ErrMedia
			}
			until = reply.RetainUntilUnixMs
			data = append(data, reply.Data...)
			now, e := clock.Observe(ctx)
			if e != nil {
				return out, e
			}
			if !now.Physical.Latest.Before(time.UnixMilli(until)) {
				return out, ErrMedia
			}
		}
		if sensorrecord.ValidateMedia(*row.capture, data) != nil {
			return out, ErrMedia
		}
		retained = append(retained, until)
		out.Media = append(out.Media, MediaReceipt{Source: owner, Reference: *ref, Data: data, RetainUntilUnixMS: until})
	}
	if len(out.Media) != 3 {
		return out, ErrMedia
	}
	if e := queryAbsent(ctx, cfg, api); e != nil {
		return out, e
	}
	out.OriginalVersionsAbsent = true
	// Account for bounded receipt serialization before the final shared clock
	// check, so expensive encoding cannot consume an unchecked retention lease.
	if raw, e := json.Marshal(out); e != nil || len(raw) > MaxResultBytes-1024 {
		return out, ErrMedia
	}
	now, e := clock.Observe(ctx)
	if e != nil {
		return out, e
	}
	if !now.Physical.Earliest.After(cfg.expiredAfter) {
		return out, ErrLive
	}
	for _, until := range retained {
		if !now.Physical.Latest.Before(time.UnixMilli(until)) {
			return out, ErrProof
		}
	}
	out.FinalPhysicalEarliestUnixNS = now.Physical.Earliest.UnixNano()
	out.FinalPhysicalLatestUnixNS = now.Physical.Latest.UnixNano()
	out.Status = "complete"
	return out, nil
}
func queryAbsent(ctx context.Context, cfg prepared, api appv1.WorldAppServiceClient) error {
	wanted := map[serviceaccess.Source]expectedRecord{}
	query := &appv1.QueryRequest{Limit: uint32(len(cfg.records))}
	for _, row := range cfg.records {
		s := source(row.expected.Reference)
		wanted[s] = row
		query.Sources = append(query.Sources, selector(s))
	}
	rpc, stop := context.WithTimeout(ctx, time.Duration(cfg.cfg.RequestTimeoutMS)*time.Millisecond)
	defer stop()
	reply, err := api.Query(rpc, query, grpc.MaxCallRecvMsgSize(192<<10), grpc.MaxRetryRPCBufferSize(0))
	if err != nil || reply == nil || len(reply.ProtoReflect().GetUnknown()) != 0 || reply.Next != "" || len(reply.SignedRecordJson) > len(cfg.records) {
		return ErrLive
	}
	seen := map[serviceaccess.Source]bool{}
	total := 0
	for _, raw := range reply.SignedRecordJson {
		total += len(raw)
		if total > 192<<10 {
			return ErrLive
		}
		r, ref, e := decode(raw)
		if e != nil {
			return ErrLive
		}
		s := source(ref)
		expected, ok := wanted[s]
		if !ok || seen[s] || r.Scope.Tenant != cfg.cfg.Tenant || r.Scope.Site != cfg.cfg.Site || r.Scope.Device != cfg.cfg.Device || ref.Key != expected.expected.Reference.Key || ref.Version.Compare(expected.expected.Reference.Version) <= 0 {
			return ErrLive
		}
		seen[s] = true
	}
	return nil
}
