package main

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/gaschange"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"time"
)

type claimStream struct {
	digest   string
	possible *world.Component
}

func (r *runtime) estimate(ctx context.Context, op string) (reply any, err error) {
	// Only a local, typed pre-submission expiry is recoverable here. Authority,
	// signature, transport and schema failures retain their normal errors.
	receipts := []map[string]any{}
	defer func() {
		if errors.Is(err, errOriginalExpired) {
			reply, err = r.expiredEstimate(err, receipts)
		}
	}()
	if op != "tick" && op != "sample" && op != "spatial" && op != "paired" {
		return nil, errors.New("estimator expects sample or tick")
	}
	if e := r.p.retry(ctx); e != nil {
		return nil, e
	}
	snapshot, e := r.snapshot(ctx)
	if e != nil {
		if r.watch == nil {
			return nil, e
		}
		now := time.Now()
		r.estimator, _ = gaschange.New(r.cfg.Public.Temporal)
		region, _, held, err := gasreplica.Evaluate(r.cfg.Public.Spatial.Gas, nil, now)
		if err != nil {
			return nil, err
		}
		change := gaschange.Result{Window: gaschange.Window{State: "unknown", Reason: "Authenticated original snapshot unavailable; new history required", Evaluated: r.cfg.Public.Spatial.Gas.Seconds(now), ValidUntil: r.cfg.Public.Spatial.Gas.Seconds(now), Traces: []gaschange.Trace{}}}
		return map[string]any{"gateway": r.cfg.Gateway, "spatial": region, "temporal": change, "held_out": held, "publications": []any{}, "transport_error": e.Error()}, nil
	}
	now := time.Now()
	change := gaschange.Result{Window: gaschange.Window{State: "unknown", Reason: "Temporal sampling paused"}}
	if op != "spatial" {
		change, e = r.estimator.Step(snapshot.Components, now)
		if e != nil {
			return nil, e
		}
	}
	if op == "sample" {
		return map[string]any{"gateway": r.cfg.Gateway, "temporal": change}, nil
	}
	region, inputs, held, e := gasreplica.Evaluate(r.cfg.Public.Spatial.Gas, snapshot.Components, now)
	if e != nil {
		return nil, e
	}
	out := map[string]any{"gateway": r.cfg.Gateway, "spatial": region, "temporal": change, "held_out": held}
	pairUsable := region.Estimate != nil && (region.State == "localized" || region.State == "ambiguous") && change.Window.State != "unknown" && change.Window.State != "warming"
	if op == "paired" && pairUsable {
		return r.publishPair(ctx, region, inputs, change, snapshot.Components, out)
	}
	if err := r.withdrawPair(ctx, snapshot.Components); err != nil {
		return nil, err
	}
	for _, kind := range []string{gasreplica.ClaimType, gaschange.ClaimType} {
		if op == "spatial" && kind == gaschange.ClaimType {
			continue
		}
		stream := r.streams[kind]
		if stream == nil {
			stream = &claimStream{}
			r.streams[kind] = stream
			// A restarted owner can withdraw its predecessor's still-live claim without
			// rebuilding temporal history or extending the predecessor's deadline.
			for _, c := range snapshot.Components {
				if c.Key.Type == kind && c.Key.Source == r.cfg.Public.Spatial.Gas.Gateways[r.cfg.Gateway] && !c.Tombstone && gasreplica.Remaining(c, now) > 0 {
					copy := c
					stream.possible = &copy
				}
			}
		}
		usable := region.Estimate != nil && (region.State == "localized" || region.State == "ambiguous")
		ids := []string{}
		if kind == gaschange.ClaimType {
			usable = change.Window.State != "unknown" && change.Window.State != "warming"
			for _, c := range change.Inputs {
				ids = append(ids, gasreplica.ComponentHash(c))
			}
		} else {
			for _, c := range inputs {
				ids = append(ids, gasreplica.ComponentHash(c))
			}
		}
		digest := gasreplica.Digest(ids)
		if !usable {
			stream.digest = ""
			if stream.possible != nil && gasreplica.Remaining(*stream.possible, time.Now()) > 0 {
				c := *stream.possible
				v, e := r.p.version(ctx)
				if e != nil {
					return nil, e
				}
				c.Version = v
				c.Tombstone = true
				c.Payload = nil
				p, e := r.p.prepare(c)
				if e != nil {
					return nil, e
				}
				r.p.pending = p
				if e = r.p.retry(ctx); e != nil {
					return nil, e
				}
				receipts = append(receipts, map[string]any{"type": kind, "withdrawn": true})
			}
			stream.possible = nil
			continue
		}
		if digest == stream.digest {
			continue
		}
		version, e := r.p.version(ctx)
		if e != nil {
			return nil, e
		}
		var c world.Component
		if kind == gaschange.ClaimType {
			c, e = gaschange.MakeClaim(r.cfg.Public.Temporal, r.cfg.Gateway, version, change)
		} else {
			c, e = gasreplica.MakeClaim(r.cfg.Public.Spatial.Gas, r.cfg.Gateway, version, region, inputs)
		}
		if e != nil {
			return nil, e
		}
		p, e := r.p.prepare(c)
		if e != nil {
			return nil, e
		}
		r.p.pending = p
		if stream.possible == nil || c.Timing.SampleTime.Add(c.TTL).After(stream.possible.Timing.SampleTime.Add(stream.possible.TTL)) {
			copy := c
			stream.possible = &copy
		}
		if e = r.p.retry(ctx); e != nil {
			return nil, e
		}
		stream.digest = digest
		receipts = append(receipts, map[string]any{"type": kind, "sha256": gasreplica.ComponentHash(c), "component": c, "signed_bytes": len(p.body)})
	}
	out["publications"] = receipts
	return out, nil
}

// expiredEstimate publishes no replacement and conservatively rebuilds temporal
// history. Earlier acknowledged separate claims are reported, not hidden; their
// original deadlines and possible-withdrawal records remain intact.
func (r *runtime) expiredEstimate(cause error, receipts []map[string]any) (any, error) {
	if !errors.Is(cause, errOriginalExpired) {
		return nil, cause
	}
	now := time.Now()
	if r.p.pending != nil && gasreplica.Remaining(r.p.pending.component, now) <= 0 {
		r.p.pending = nil
	}
	for _, stream := range r.streams {
		stream.digest = ""
	}
	var err error
	r.estimator, err = gaschange.New(r.cfg.Public.Temporal)
	if err != nil {
		return nil, err
	}
	region, _, held, err := gasreplica.Evaluate(r.cfg.Public.Spatial.Gas, nil, now)
	if err != nil {
		return nil, err
	}
	seconds := r.cfg.Public.Spatial.Gas.Seconds(now)
	change := gaschange.Result{Window: gaschange.Window{State: "unknown", Reason: "Original support expired before submission; fresh history required", Evaluated: seconds, ValidUntil: seconds, Traces: []gaschange.Trace{}}}
	return map[string]any{"gateway": r.cfg.Gateway, "spatial": region, "temporal": change, "held_out": held, "publications": receipts, "publication_complete": false, "expired_payload_submitted": false, "publication_error": cause.Error()}, nil
}
