package main

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/gaschange"
	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/incidentchange"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"time"
)

func (r *runtime) pairStream(records []world.Component) *claimStream {
	stream := r.streams[incidentchange.PairType]
	if stream == nil {
		stream = &claimStream{}
		r.streams[incidentchange.PairType] = stream
		for _, c := range records {
			if c.Key.Type == incidentchange.PairType && c.Key.Source == r.cfg.Public.Spatial.Gas.Gateways[r.cfg.Gateway] && !c.Tombstone && gasreplica.Remaining(c, time.Now()) > 0 {
				copy := c
				stream.possible = &copy
			}
		}
	}
	return stream
}
func (r *runtime) withdrawPair(ctx context.Context, records []world.Component) error {
	stream := r.pairStream(records)
	if stream.digest != "" || stream.possible != nil {
		for kind, other := range r.streams {
			if kind != incidentchange.PairType {
				other.digest = ""
			}
		}
	}
	stream.digest = ""
	if stream.possible == nil || gasreplica.Remaining(*stream.possible, time.Now()) <= 0 {
		stream.possible = nil
		return nil
	}
	c := *stream.possible
	v, err := r.p.version(ctx)
	if err != nil {
		return err
	}
	c.Version = v
	c.Tombstone = true
	c.Payload = nil
	pending, err := r.p.prepare(c)
	if err != nil {
		return err
	}
	r.p.pending = pending
	if err = r.p.retry(ctx); err != nil {
		return err
	}
	stream.possible = nil
	return nil
}
func (r *runtime) publishPair(ctx context.Context, region gasfield.Result, inputs []world.Component, change gaschange.Result, records []world.Component, out map[string]any) (any, error) {
	if err := r.refreshTrust(); err != nil {
		return nil, err
	}
	stream := r.pairStream(records)
	ids := []string{change.Window.InputDigest}
	for _, c := range inputs {
		ids = append(ids, gasreplica.ComponentHash(c))
	}
	digest := gasreplica.Digest(ids)
	if digest == stream.digest {
		out["publications"] = []any{}
		return out, nil
	}
	version, err := r.p.version(ctx)
	if err != nil {
		return nil, err
	}
	gas, err := gasreplica.MakeClaim(r.cfg.Public.Spatial.Gas, r.cfg.Gateway, version, region, inputs)
	if err != nil {
		return nil, err
	}
	trend, err := gaschange.MakeClaim(r.cfg.Public.Temporal, r.cfg.Gateway, version, change)
	if err != nil {
		return nil, err
	}
	gasSigned, err := r.p.prepare(gas)
	if err != nil {
		return nil, err
	}
	trendSigned, err := r.p.prepare(trend)
	if err != nil {
		return nil, err
	}
	members := [][]byte{gasSigned.body, trendSigned.body}
	now := time.Now()
	if err := pairMembersCurrent(members, now, r.verifyMembers); err != nil {
		return nil, err
	}
	carrier, err := incidentchange.MakePair(r.cfg.Public, r.cfg.Gateway, version, members, now, r.verifyMembers)
	if err != nil {
		return nil, err
	}
	pending, err := r.p.prepare(carrier)
	if err != nil {
		return nil, err
	}
	r.p.pending = pending
	stream.possible = &carrier
	if err = r.p.retry(ctx); err != nil {
		return nil, err
	}
	stream.digest = digest
	out["publications"] = []map[string]any{{"type": incidentchange.PairType, "sha256": gasreplica.ComponentHash(carrier), "component": carrier, "signed_bytes": len(pending.body), "gas_sha256": gasreplica.ComponentHash(gas), "change_sha256": gasreplica.ComponentHash(trend)}}
	return out, nil
}

// A locally constructed pair can expire while obtaining its durable epoch or
// signing members. Verify intact member authority before classifying only that
// deadline boundary as recoverable; MakePair retains all domain validation.
func pairMembersCurrent(members [][]byte, now time.Time, verify incidentchange.PairVerifier) error {
	if len(members) != 2 || verify == nil {
		return errors.New("two verified pair members required")
	}
	records := make([]authority.SignedRecord, 2)
	for i, raw := range members {
		if err := authority.DecodeStrictLimit(raw, &records[i], 48<<10); err != nil {
			return err
		}
	}
	if err := verify(records); err != nil {
		return err
	}
	for _, record := range records {
		if gasreplica.Remaining(record.Component, now) <= 0 {
			return errOriginalExpired
		}
	}
	return nil
}
