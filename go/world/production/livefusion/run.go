package livefusion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appclock"
	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func Run(ctx context.Context, config Config, api appv1.WorldAppServiceClient) (Result, error) {
	return run(ctx, config, api, nil)
}

func run(ctx context.Context, config Config, api appv1.WorldAppServiceClient, persistent *Session) (out Result, err error) {
	out = Result{Version: 1, RunID: config.RunID, Status: "failed"}
	native := persistent != nil && persistent.native
	if native {
		out.InputAuthority = "native_agent_admission"
	}
	stage := "configuration"
	var s *session
	diagnostics := &Diagnostics{}
	started := time.Now()
	var current *AttemptDiagnostics
	defer func() {
		diagnostics.ElapsedNS = int64(time.Since(started))
		if current != nil {
			current.ElapsedNS = int64(time.Since(current.started))
		}
		if err != nil || out.Status != "complete" {
			out.Diagnostics = diagnostics
		}
	}()
	defer func() {
		if err != nil {
			out.FailureStage = stage
			out.ErrorCode = failureCode(err)
			if s != nil && s.failureCode != "" {
				out.ErrorCode = s.failureCode
			}
			if errors.Is(err, ErrUncertain) {
				out.Status = "unknown_outcome"
				out.ErrorCode = "mutation_outcome_unknown"
			} else if errors.Is(err, ErrConfig) {
				out.ErrorCode = "configuration_refused"
			}
		}
	}()
	config, err = config.normalized()
	if err != nil || ctx == nil || api == nil {
		return out, ErrConfig
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(config.DurationMS)*time.Millisecond)
	defer cancel()
	var clock *diagnosticClock
	var certificates *wendycert.DeviceVerifier
	var window *candidateWindow
	if persistent != nil && persistent.state != nil {
		s, certificates, window = persistent.state, persistent.certificates, persistent.window
		clock = s.clock
		clock.stats, clock.current = diagnostics, nil
		s.failureCode = ""
	} else {
		baseClock, e := appclock.New(api, appclock.Config{Tenant: config.Tenant, Site: config.Site, Device: config.Device, App: config.App, Service: config.Service, ExpectedGeneration: config.ExpectedGeneration, ExecutionDomain: "simulation", SampleClockDomain: "simulation:" + config.RunID, MaxRoundTrip: min(time.Duration(config.RequestTimeoutMS)*time.Millisecond, appv1.MaxClockResponseAge), MaxWidth: time.Duration(config.ClockMaxWidthMS) * time.Millisecond, DriftPPM: 100})
		if e != nil {
			return out, ErrConfig
		}
		clock = &diagnosticClock{base: baseClock, stats: diagnostics}
		s = &session{cfg: config, api: api, clock: clock, sources: map[string]Source{}, bindings: map[string]sourceBinding{}}
		for _, source := range config.Sources {
			s.sources[source.SensorID] = source
		}
		certificates, err = wendycert.NewDevice(wendycert.Config{Tenant: config.Tenant, Roots: config.RootsDER})
		if err == nil && !native {
			s.verifier, err = serviceorigin.NewBatchVerifier(serviceorigin.BatchVerifierConfig{Tenant: config.Tenant, Site: config.Site, ExecutionDomain: "simulation", Certificates: certificates, Clock: clock, Authorize: s.authorizeBatch, MaxRecords: len(config.Sources), MaxBytes: 1 << 20})
		}
		if err == nil {
			window, err = newCandidateWindow(s, time.Duration(config.MaxSkewMS)*time.Millisecond)
		}
		if err != nil {
			clock.Close()
			return out, ErrConfig
		}
		if persistent != nil {
			persistent.state, persistent.certificates, persistent.window = s, certificates, window
			window.maxRetired = config.MaxTicks
			window.currentRemoteOnly = native
		}
	}
	if persistent == nil {
		defer clock.Close()
	}
	stage = "initial_clock"
	if _, err = clock.Observe(ctx); err != nil {
		return out, err
	}
	for attempt := 1; attempt <= config.MaxAttempts; attempt++ {
		if current != nil {
			current.ElapsedNS = int64(time.Since(current.started))
		}
		current = &AttemptDiagnostics{Attempt: attempt, started: time.Now()}
		diagnostics.Attempts = append(diagnostics.Attempts, current)
		clock.current = current
		out.Attempts = attempt
		out.Inputs = nil
		out.Media = nil
		out.Evaluation = nil
		stage = "query_candidates"
		proofs, e := s.query(ctx, s.sources)
		if e != nil {
			current.QueryFailure = s.failureCode
			// Discovery has not granted authority or attempted a mutation.
			// A transient failed read consumes this attempt; start a complete
			// fresh query after the existing bounded poll delay. Verification,
			// media reads and publication do not take this retry path.
			if errors.Is(e, errQueryUnavailable) && ctx.Err() == nil && persistent != nil {
				out.Status = "waiting"
				return out, nil
			}
			if errors.Is(e, errQueryUnavailable) && ctx.Err() == nil && attempt < config.MaxAttempts {
				current.QueryRetryScheduled = true
				s.failureCode = ""
				goto nextAttempt
			}
			return out, e
		}
		current.captureRange(proofs)
		{
			stage = "match_candidates"
			started := time.Now()
			selected, matchErr := window.updateAndSelect(proofs)
			current.MatchingElapsedNS = int64(time.Since(started))
			if matchErr != nil {
				return out, matchErr
			}
			if s.completeProofs(selected) {
				current.recordSelection(proofs, selected)
				proofs = selected
			} else {
				// A complete latest query cannot bypass retirement when the
				// window found no eligible tuple (including an exact repeat).
				current.EvaluationOutcome = "no_eligible_tuple"
				out.Evaluation = &fusion.Result{State: fusion.InsufficientEvidence, Reason: "no unevaluated coherent capture tuple"}
				goto nextAttempt
			}
		}
		if s.completeProofs(proofs) {
			var envelopes [][]byte
			for _, p := range proofs {
				out.Inputs = append(out.Inputs, ProofReceipt{Reference: p.reference, EnvelopeJSON: bytes.Clone(p.raw)})
				envelopes = append(envelopes, bytes.Clone(p.raw))
			}
			if !coherent(proofs, time.Duration(config.MaxSkewMS)*time.Millisecond) {
				out.Evaluation = &fusion.Result{State: fusion.InsufficientEvidence, Reason: "complete capture intervals exceed configured skew"}
				goto nextAttempt
			}
			var sampleClockError error
			cfg := fusion.Config{Tenant: config.Tenant, Site: config.Site, RunID: config.RunID, ClockDomain: "simulation:" + config.RunID, Frame: config.Frame, MaxSkew: time.Duration(config.MaxSkewMS) * time.Millisecond, Sources: map[string]world.Source{}, RequiredSensors: map[string]string{}, OptionalSensors: map[string]string{}, Now: func() time.Time {
				v, e := clock.SampleNow()
				if e != nil {
					sampleClockError = e
					return time.Time{}
				}
				return v
			}}
			for _, source := range config.Sources {
				kind := "lidar"
				switch source.SensorID {
				case "camera-a", "camera-b":
					kind = "camera"
				case "gas-1":
					kind = "gas"
				case "audio-1":
					kind = "audio"
				}
				if kind == "lidar" {
					cfg.OptionalSensors[source.SensorID] = kind
				} else {
					cfg.RequiredSensors[source.SensorID] = kind
				}
				cfg.Sources[source.SensorID] = world.Source{Device: source.Device, App: source.App, Service: source.Service, Stream: source.SensorID}
			}
			var mediaCache *nativeMediaCache
			if native {
				mediaCache = persistent.mediaCache
			}
			loader := s.mediaLoader(proofs, &out.Media, mediaCache)
			var mediaError error
			selectionCtx := ctx
			if persistent != nil {
				selectionCtx = context.WithValue(ctx, captureReadKey{}, &captureReadState{})
			}
			// Independent mode verifies originals in both evaluation rounds;
			// native mode projects the broker-admitted originals once. Either
			// projection is used only after the shared final time/media checks.
			var finalVerified []serviceorigin.VerifiedRecord
			verifiedPasses := 0
			stage = "evaluate_capture"
			verify := func(ctx context.Context, signed []serviceorigin.SignedRecord) ([]serviceorigin.VerifiedRecord, error) {
				stage = "verify_input"
				checked, err := s.verifyBatch(ctx, signed)
				if err == nil {
					finalVerified = checked
					verifiedPasses++
					stage = "evaluate_capture"
				}
				return checked, err
			}
			load := func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
				stage = "read_media"
				data, e := loader(ctx, ref)
				if e != nil {
					mediaError = e
				} else {
					stage = "evaluate_capture"
				}
				return data, e
			}
			var result fusion.Result
			options := fusioncapture.Options{Physical: clock, Diagnostics: &current.Physical}
			if native {
				result, e = fusioncapture.EvaluateAdmitted(selectionCtx, cfg, options, envelopes, func(ctx context.Context, signed []serviceorigin.SignedRecord) ([]serviceorigin.Record, error) {
					stage = "project_admitted_inputs"
					projected, err := s.projectAdmitted(ctx, proofs, signed)
					if err == nil {
						finalVerified = make([]serviceorigin.VerifiedRecord, len(projected))
						for i, record := range projected {
							finalVerified[i] = serviceorigin.VerifiedRecord{Record: record, Bytes: bytes.Clone(signed[i].Record)}
						}
						stage = "evaluate_capture"
					}
					return projected, err
				}, load)
			} else {
				result, e = fusioncapture.EvaluateBatch(selectionCtx, cfg, options, envelopes, verify, load)
			}
			if mediaError != nil {
				return out, mediaError
			}
			if sampleClockError != nil {
				return out, sampleClockError
			}
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			if e != nil {
				reason, checked := s.expiredSelection(selectionCtx, proofs, e)
				if reason != "" {
					if retireErr := window.retire(proofs); retireErr != nil {
						return out, retireErr
					}
					current.EvaluationOutcome, current.RetryReason = "capture_expired", reason
					out.Evaluation = &fusion.Result{State: fusion.InsufficientEvidence, Reason: reason}
					goto nextAttempt
				}
				e = checked
				var stale *fusioncapture.StaleCaptureError
				if errors.As(e, &stale) && (stale.Reason() == "physical_capture_expired" || stale.Reason() == "sample_capture_expired") && ctx.Err() == nil {
					if retireErr := window.retire(proofs); retireErr != nil {
						return out, retireErr
					}
					current.EvaluationOutcome, current.RetryReason = "capture_expired", stale.Reason()
					out.Evaluation = &fusion.Result{State: fusion.InsufficientEvidence, Reason: stale.Reason()}
					goto nextAttempt
				}
				return out, e
			}
			if !native && verifiedPasses != 2 {
				return out, ErrSource
			}
			out.Evaluation = &result
			current.EvaluationOutcome = result.State
			if result.State == fusion.HazardDetected {
				stage = "validate_result_inputs"
				inputs, e := s.buildValidated(selectionCtx, proofs, finalVerified)
				if e != nil {
					return out, e
				}
				stage = "result_clock"
				observation, e := clock.Observe(ctx)
				if e != nil {
					return out, e
				}
				for _, input := range inputs {
					if !observation.SampleTime.Before(input.SampleValidUntil) || !observation.Physical.Latest.Before(input.PhysicalValidUntil) {
						// Original signing authority expiration is terminal. Only
						// original capture bounds may consume another attempt.
						for _, proof := range proofs {
							if !observation.Physical.Latest.Before(time.UnixMilli(proof.record.ExpiresAtUnixMS)) {
								return out, ErrSource
							}
						}
						if e = window.retire(proofs); e != nil {
							return out, e
						}
						current.RetryReason = "sample_capture_expired"
						if !observation.Physical.Latest.Before(input.PhysicalValidUntil) {
							current.RetryReason = "physical_capture_expired"
						}
						goto nextAttempt
					}
				}
				stage = "build_result"
				publication, e := fusioncapture.BuildPublication(result, inputs, config.ResultStream, observation.Physical.Earliest.Add(time.Duration(config.RetainForMS)*time.Millisecond))
				if e != nil {
					return out, e
				}
				if native {
					fresh, reserveErr := persistent.reserveNativePublication(proofs, result.InputDigest)
					if reserveErr != nil {
						return out, reserveErr
					}
					if !fresh {
						current.EvaluationOutcome = "already_attempted_input_digest"
						out.Evaluation = &fusion.Result{State: fusion.InsufficientEvidence, Reason: "inference input digest already attempted"}
						goto nextAttempt
					}
				}
				stage = "publish_result"
				if e = s.publish(ctx, publication, inputs, certificates, &out); e != nil {
					return out, e
				}
				if persistent != nil && !native {
					if e = window.retire(proofs); e != nil {
						return out, e
					}
				}
				out.Status = "complete"
				return out, nil
			}
			if result.State == fusion.NotDetected {
				if persistent != nil {
					if e = window.retire(proofs); e != nil {
						return out, e
					}
				}
				out.Status = "not_detected"
				return out, nil
			}
			if result.State == fusion.InsufficientEvidence {
				if e = window.retire(proofs); e != nil {
					return out, e
				}
				current.RetryReason = "verified_insufficient_evidence"
			}
		}
	nextAttempt:
		if attempt < config.MaxAttempts {
			timer := time.NewTimer(time.Duration(config.PollIntervalMS) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return out, ctx.Err()
			case <-timer.C:
			}
		}
	}
	out.Status = "insufficient_evidence"
	return out, nil
}

func (s *session) publish(ctx context.Context, request serviceorigin.PublishRequest, inputs []resultrecord.ValidatedInput, certificates *wendycert.DeviceVerifier, out *Result) error {
	diagnostic := &PublicationDiagnostics{}
	s.clock.stats.Publication = diagnostic
	started := time.Now()
	defer func() { diagnostic.ElapsedNS = int64(time.Since(started)) }()
	diagnostic.FailureStage = "request_encode"
	wire, err := appv1.EncodePublish(request)
	if err != nil {
		return err
	}
	diagnostic.FailureStage = "request_marshal"
	out.RequestJSON, err = protojson.MarshalOptions{UseProtoNames: true}.Marshal(wire)
	if err != nil {
		return err
	}
	diagnostic.FailureStage = "request_bounds"
	if raw, e := json.Marshal(out); e != nil || len(raw) > MaxResultBytes-(1<<20) {
		return ErrConfig
	}
	// Exactly one mutation attempt. Everything after entering this RPC may
	// represent a committed result even if transport or verification fails.
	rpc, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.RequestTimeoutMS)*time.Millisecond)
	diagnostic.Attempts = 1
	diagnostic.FailureStage = "rpc"
	rpcStarted := time.Now()
	ack, err := s.api.Publish(rpc, wire, grpc.MaxCallRecvMsgSize(256<<10), grpc.MaxRetryRPCBufferSize(0))
	diagnostic.RPCElapsedNS = int64(time.Since(rpcStarted))
	diagnostic.GRPCCode = status.Code(err).String()
	cancel()
	postAckStarted := time.Now()
	defer func() { diagnostic.PostAckElapsedNS = int64(time.Since(postAckStarted)) }()
	if ack != nil {
		diagnostic.AckPresent = true
		diagnostic.AckAccepted = ack.Accepted
		diagnostic.ProofBytes = len(ack.SignedRecordJson)
		// Preserve the exact proof field even when the RPC or acknowledgment is
		// refused. This is unverified diagnostic data, not evidence of success.
		if len(ack.SignedRecordJson) <= 256<<10 {
			out.SignedRecordJSON = bytes.Clone(ack.SignedRecordJson)
			if len(ack.SignedRecordJson) > 0 {
				out.ProofSHA256 = digest(ack.SignedRecordJson)
			}
		}
	}
	if err != nil {
		return ErrUncertain
	}
	diagnostic.FailureStage = "ack_shape"
	if ack == nil || !ack.Accepted || len(ack.ProtoReflect().GetUnknown()) != 0 || len(ack.SignedRecordJson) > 256<<10 {
		return ErrUncertain
	}
	diagnostic.FailureStage = "ack_decode"
	signed, record, err := decodeEnvelope(ack.SignedRecordJson)
	if err != nil {
		return ErrUncertain
	}
	out.RecordSHA256 = digest(signed.Record)
	diagnostic.FailureStage = "ack_request_match"
	encoded, err := appv1.EncodePublish(record.Publication)
	if err != nil || !proto.Equal(encoded, wire) {
		return ErrUncertain
	}
	diagnostic.FailureStage = "ack_verifier_setup"
	verifier, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: s.cfg.Tenant, Site: s.cfg.Site, ExecutionDomain: "simulation", Certificates: certificates, Clock: s.clock, Authorize: func(ctx context.Context, claims serviceorigin.DelegationClaims, device wendycert.DeviceIdentity) error {
		observation, e := s.clock.Observe(ctx)
		if e != nil || device.Principal != s.cfg.Device || claims.Scope.Device != s.cfg.Device || claims.Scope.App != s.cfg.App || claims.Scope.Service != s.cfg.Service || claims.Scope.DeploymentGeneration != observation.DeploymentGeneration || len(claims.Scope.Publish) != 1 || claims.Scope.Publish[0] != (serviceorigin.PublishScope{ComponentType: resultrecord.ComponentType, Stream: s.cfg.ResultStream}) {
			return ErrSource
		}
		return nil
	}})
	if err != nil {
		return ErrUncertain
	}
	diagnostic.FailureStage = "ack_verify"
	verified, err := verifier.Verify(ctx, signed)
	if err != nil {
		diagnostic.FailureCode = failureCode(err)
		return ErrUncertain
	}
	if !bytes.Equal(verified.Bytes, signed.Record) {
		return ErrUncertain
	}
	diagnostic.FailureStage = "ack_result_validation"
	if _, err = resultrecord.ValidatePublication(record.Publication, inputs); err != nil {
		return ErrUncertain
	}
	// The acknowledgment proves scoped agent admission, not durable hardware
	// action or independent correctness of this synthetic inference model.
	diagnostic.FailureStage = "receipt_bounds"
	if data, e := json.Marshal(out); e != nil || len(data) > MaxResultBytes {
		return ErrUncertain
	}
	out.LocallyVerified = true
	diagnostic.FailureStage = ""
	return nil
}

// coherent only rejects unsuitable candidates before costly current-proof
// checks. Passing this gate grants no authority and does not skip verification.
func coherent(proofs []inputProof, skew time.Duration) bool {
	var earliest, latest time.Time
	for _, p := range proofs {
		sample, err := p.capture.Sample()
		if err != nil {
			return false
		}
		end := p.record.Publication.Timing.SampleTime
		start := end
		if sample.Audio != nil {
			start = start.Add(-time.Duration(sample.Audio.DurationNS))
		}
		if earliest.IsZero() || start.Before(earliest) {
			earliest = start
		}
		if latest.IsZero() || end.After(latest) {
			latest = end
		}
	}
	return len(proofs) >= 4 && len(proofs) <= serviceaccess.MaxQueryRecords && !earliest.IsZero() && latest.Sub(earliest) <= skew
}
func failureCode(err error) string {
	switch {
	case errors.Is(err, ErrSource):
		return "current_source_refused"
	case errors.Is(err, ErrMedia), errors.Is(err, fusioncapture.ErrCaptureMedia):
		return "evidence_refused"
	case errors.Is(err, serviceorigin.ErrClock):
		return "signature_clock_refused"
	case errors.Is(err, serviceorigin.ErrExpired):
		return "signature_expired"
	case errors.Is(err, serviceorigin.ErrDenied):
		return "signature_or_authority_refused"
	case errors.Is(err, appclock.ErrFaulted):
		return "agent_clock_faulted"
	case errors.Is(err, appclock.ErrUnavailable):
		return "agent_clock_unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "operation_deadline"
	case errors.Is(err, context.Canceled):
		return "operation_canceled"
	}
	switch err.Error() {
	case "capture exceeds physical age bound":
		return "capture_physical_age_refused"
	case "authenticated physical capture clock unavailable":
		return "capture_clock_unavailable"
	case "authenticated physical capture clock regressed":
		return "capture_clock_regressed"
	case "invalid current time":
		return "sample_clock_unavailable"
	}
	return "evaluation_refused"
}
