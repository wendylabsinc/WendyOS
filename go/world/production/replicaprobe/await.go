package replicaprobe

import (
	"bytes"
	"context"
	"errors"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func queryCode(err error) codes.Code {
	if errors.Is(err, context.DeadlineExceeded) {
		return codes.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return codes.Canceled
	}
	return status.Code(err)
}

func (p *Probe) queryRequest() *appv1.QueryRequest {
	return &appv1.QueryRequest{Sources: []*appv1.SourceSelector{{Device: p.cfg.PeerDevice,
		App: App, Service: Service, ComponentType: Component, Stream: Stream}}, Limit: serviceaccess.MaxQueryRecords}
}

func (p *Probe) queryReply(reply *appv1.QueryReply) ([]byte, []Original, error) {
	if reply == nil || len(reply.ProtoReflect().GetUnknown()) != 0 || reply.Next != "" || len(reply.SignedRecordJson) > 1 || proto.Size(reply) > serviceaccess.MaxQueryBytes {
		return nil, []Original{}, ErrResponse
	}
	originals := []Original{}
	for _, raw := range reply.SignedRecordJson {
		proof, record, err := p.original(raw, p.cfg.PeerDevice)
		if err != nil || record.Publication.Tombstone {
			return nil, []Original{}, ErrResponse
		}
		originals = append(originals, Original{bytes.Clone(raw), digest(raw), digest(proof.Record)})
	}
	raw, err := protojson.Marshal(reply)
	if err != nil {
		return nil, []Original{}, ErrResponse
	}
	return raw, originals, nil
}

func (p *Probe) awaitQuery(parent context.Context, command Command, out *Receipt) error {
	started := time.Now()
	defer func() { out.AwaitElapsedNS = int64(time.Since(started)) }()
	ctx, cancel := context.WithTimeout(parent, AwaitBudget)
	defer cancel()
	for attempt := 1; attempt <= MaxAwaitAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Every attempt owns a new observation. Never pair an old successful
		// reply with a later refused clock or query.
		out.Clock, out.RequestJSON, out.ReplyJSON, out.RPCCode = nil, nil, nil, ""
		out.Originals = []Original{}
		entry := QueryAttempt{Attempt: attempt}
		attemptStart := time.Now()
		finish := func(outcome string) {
			entry.Outcome, entry.ElapsedNS = outcome, int64(time.Since(attemptStart))
			out.Attempts = append(out.Attempts, entry)
		}
		observed, err := p.clock.Observe(ctx)
		if err != nil {
			finish("clock_refused")
			return err
		}
		out.Clock = &ClockReceipt{observed.SampleTime.UnixNano(), observed.SampleClockDomain,
			observed.DeploymentGeneration, observed.ServerBootID, observed.Physical.Earliest.UnixNano(),
			observed.Physical.Latest.UnixNano(), int64(observed.RoundTrip)}
		entry.Clock = out.Clock
		if err = ctx.Err(); err != nil {
			finish("canceled")
			return err
		}
		request := p.queryRequest()
		out.RequestJSON, _ = protojson.Marshal(request)
		call, stop := context.WithTimeout(ctx, time.Duration(p.cfg.RequestTimeoutMS)*time.Millisecond)
		reply, rpcErr := p.client.Query(call, request)
		callErr := call.Err()
		stop()
		out.RPCCode = queryCode(rpcErr).String()
		entry.RPCCode = out.RPCCode
		if err = ctx.Err(); err != nil {
			finish("canceled")
			return err
		}
		// A backend that returns a late success cannot turn an expired call
		// into a matching read. No response or authority is cached for retry.
		if rpcErr == nil && callErr != nil {
			rpcErr = callErr
			out.RPCCode, entry.RPCCode = codes.DeadlineExceeded.String(), codes.DeadlineExceeded.String()
		}
		matched := false
		if rpcErr != nil {
			switch queryCode(rpcErr) {
			case codes.PermissionDenied, codes.Unavailable, codes.DeadlineExceeded:
				finish("rpc_retryable")
			default:
				finish("rpc_refused")
				return rpcErr
			}
		} else {
			out.ReplyJSON, out.Originals, err = p.queryReply(reply)
			if err != nil {
				finish("response_refused")
				return err
			}
			entry.ReplySHA256 = digest(out.ReplyJSON)
			if len(out.Originals) == 1 {
				entry.EnvelopeSHA256 = out.Originals[0].EnvelopeSHA256
			}
			switch command.Expect {
			case "ready":
				matched = true
			case "absent":
				matched = len(out.Originals) == 0
			case "present":
				matched = len(out.Originals) == 1 && entry.EnvelopeSHA256 == command.EnvelopeSHA256
			}
			if err = ctx.Err(); err != nil {
				finish("canceled")
				return err
			}
			if matched {
				finish("matched")
			} else {
				finish("predicate_mismatch")
			}
			// Keep failure diagnostics bounded too. If a valid reply cannot
			// fit alongside its attempt history, retain hashes and refuse it.
			if _, err = encoded(out, MaxReceiptBytes-1024); err != nil {
				out.Attempts[len(out.Attempts)-1].Outcome = "response_refused"
				out.ReplyJSON, out.Originals = nil, []Original{}
				return ErrResponse
			}
			if err = ctx.Err(); err != nil {
				out.Attempts[len(out.Attempts)-1].Outcome = "canceled"
				return err
			}
			if matched {
				return nil
			}
		}
		if attempt == MaxAwaitAttempts {
			return ErrAwait
		}
		timer := time.NewTimer(AwaitInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ErrAwait
}
