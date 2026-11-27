// world-incident-change publishes an exact-original spatial/temporal join.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/gaschange"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/incidentchange"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type runtimeConfig struct {
	Public    incidentchange.Config `json:"public"`
	Gateway   string                `json:"gateway,omitempty"`
	Reader    string                `json:"reader,omitempty"`
	Client    client.Config         `json:"client"`
	TrustPath string                `json:"trust_path,omitempty"`
	Watch     bool                  `json:"watch,omitempty"`
}
type runtime struct {
	cfg              runtimeConfig
	p                *publisher
	view             *incidentchange.Session
	lastInput        string
	possible         *world.Component
	estimator        *gaschange.Session
	streams          map[string]*claimStream
	observedResult   *incidentchange.Result
	observedRecords  []world.Component
	publishedResult  *incidentchange.Result
	publishedRecords []world.Component
	memberVerifier   *authority.Verifier
	trustedRoot      []byte
	watch            *snapshotWatch
}

func newRuntime(cfg runtimeConfig) (*runtime, error) {
	if e := cfg.Public.Validate(); e != nil {
		return nil, e
	}
	source, ok := cfg.Public.Spatial.Readers[cfg.Reader]
	if cfg.Gateway != "" {
		source, ok = cfg.Public.Spatial.Gas.Gateways[cfg.Gateway]
	}
	if !ok || cfg.Client.Grant.Grant.Source != source {
		return nil, errors.New("commissioned independent reader source required")
	}
	p, e := newPublisher(cfg.Client)
	if e != nil {
		return nil, e
	}
	view, e := incidentchange.New(cfg.Public)
	r := &runtime{cfg: cfg, p: p, view: view, streams: map[string]*claimStream{}}
	if cfg.TrustPath != "" {
		if err := r.refreshTrust(); err != nil {
			return nil, err
		}
		r.view, e = incidentchange.NewWithPairVerifier(cfg.Public, r.verifyMembers)
	}
	if cfg.Gateway != "" {
		r.estimator, e = gaschange.New(cfg.Public.Temporal)
	}
	return r, e
}
func (r *runtime) step(ctx context.Context, op string) (any, error) {
	if op == "query" {
		return r.p.api.Query(ctx)
	}
	if op == "duplicates" {
		if r.p.last == nil {
			return nil, errors.New("no original publication")
		}
		return map[string]bool{"resent_original": true}, r.p.send(ctx, r.p.last)
	}
	if r.estimator != nil {
		return r.estimate(ctx, op)
	}
	if op == "publish" {
		if r.observedResult == nil {
			return nil, errors.New("observe an exact view before publication")
		}
		return r.publishView(ctx, *r.observedResult, r.observedRecords)
	}
	if op != "tick" && op != "observe" {
		return nil, errors.New("reader expects tick, observe, publish, query or duplicates")
	}
	snapshot, e := r.snapshot(ctx)
	if e != nil {
		if r.watch == nil {
			return nil, e
		}
		current, err := r.view.Step(nil, time.Now())
		if err != nil {
			return nil, err
		}
		r.observedResult = &current
		r.observedRecords = nil
		return map[string]any{"reader": r.cfg.Reader, "node": r.cfg.Client.Audience, "result": current, "published": false, "transport_error": e.Error()}, nil
	}
	if e = r.refreshTrust(); e != nil {
		return nil, e
	}
	result, e := r.view.Step(snapshot.Components, time.Now())
	if e != nil {
		return nil, e
	}
	r.observedResult = &result
	r.observedRecords = snapshot.Components
	if op == "observe" {
		return map[string]any{"reader": r.cfg.Reader, "node": r.cfg.Client.Audience, "result": result, "published": false}, nil
	}
	return r.publishView(ctx, result, snapshot.Components)
}

func (r *runtime) publishView(ctx context.Context, result incidentchange.Result, records []world.Component) (any, error) {
	if err := r.p.retry(ctx); err != nil && !errors.Is(err, errOriginalExpired) {
		return nil, err
	}
	out := map[string]any{"reader": r.cfg.Reader, "node": r.cfg.Client.Audience, "result": result, "published": false}
	if err := r.refreshTrust(); err != nil {
		return nil, err
	}
	for _, carrier := range result.Carriers {
		for _, record := range records {
			if record.Key.Type == incidentchange.PairType && gasreplica.ComponentHash(record) == carrier.Hash {
				if _, err := incidentchange.ValidatePair(r.cfg.Public, record, time.Now(), r.verifyMembers); err != nil {
					return r.expiredPublication(out, records, "paired original authority or lifetime no longer current")
				}
			}
		}
	}

	if result.State != "supported_rise" && result.State != "non_rising" {
		r.lastInput = ""
		if r.possible != nil && gasreplica.Remaining(*r.possible, time.Now()) > 0 {
			c := *r.possible
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
				if errors.Is(e, errOriginalExpired) {
					return r.expiredPublication(out, records, e.Error())
				}
				return nil, e
			}
			out["withdrawn"] = true
		}
		r.possible = nil
		return out, nil
	}
	if r.lastInput == result.InputDigest {
		return out, nil
	}
	version, e := r.p.version(ctx)
	if e != nil {
		return nil, e
	}
	c, e := incidentchange.Component(r.cfg.Public, r.cfg.Reader, version, result)
	if e != nil {
		return nil, e
	}
	if gasreplica.Remaining(c, time.Now()) <= 0 {
		return r.expiredPublication(out, records, "original joined support expired before signing")
	}
	pending, e := r.p.prepare(c)
	if e != nil {
		return nil, e
	}
	r.p.pending = pending
	if r.possible == nil || c.Timing.SampleTime.Add(c.TTL).After(r.possible.Timing.SampleTime.Add(r.possible.TTL)) {
		r.possible = &c
	}
	if e = r.p.retry(ctx); e != nil {
		if errors.Is(e, errOriginalExpired) || gasreplica.Remaining(c, time.Now()) <= 0 {
			return r.expiredPublication(out, records, e.Error())
		}
		return nil, e
	}
	r.lastInput = result.InputDigest
	copyResult := result
	r.publishedResult = &copyResult
	if err := r.retainPublishedRecords(records); err != nil {
		return nil, err
	}
	out["published"] = true
	out["component"] = c
	out["sha256"] = gasreplica.ComponentHash(c)
	out["signed_bytes"] = len(pending.body)
	return out, nil
}

// Re-evaluate the same originals after a publication deadline race. Never
// return the earlier current result or renew support merely to complete a POST.
func (r *runtime) expiredPublication(out map[string]any, records []world.Component, reason string) (any, error) {
	r.p.pending = nil
	r.lastInput = ""
	current, err := r.view.Step(records, time.Now())
	if err != nil {
		return nil, err
	}
	out["result"] = current
	out["published"] = false
	out["publication_error"] = reason
	return out, nil
}

func load(path string, out any) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	if e != nil {
		return e
	}
	return authority.DecodeStrictLimit(raw, out, 256<<10)
}
func run() error {
	role := flag.String("role", "reader", "setup, estimator or reader")
	path := flag.String("config", "", "private config")
	out := flag.String("out", "", "fresh private setup directory")
	base := flag.Int("base-port", 29880, "six loopback ports")
	inspection := flag.Bool("inspection", false, "commission existing explicit simulation inspection workflow")
	flag.Parse()
	if *role == "setup" {
		var c incidentchange.Config
		if e := load(*path, &c); e != nil {
			return e
		}
		return setupOptions(*out, c, *base, *inspection)
	}
	if *role != "reader" && *role != "estimator" {
		return errors.New("role must be setup or reader")
	}
	var cfg runtimeConfig
	if e := load(*path, &cfg); e != nil {
		return e
	}
	if (*role == "estimator") != (cfg.Gateway != "") {
		return errors.New("role and commissioned configuration differ")
	}
	r, e := newRuntime(cfg)
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r.startWatch(ctx)
	defer r.stopWatch()
	go func() { <-ctx.Done(); _ = os.Stdin.Close() }()
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), 8192)
	enc := json.NewEncoder(os.Stdout)
	for scan.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		var q struct {
			Op          string `json:"op"`
			InputDigest string `json:"input_digest,omitempty"`
		}
		var v any
		e := authority.DecodeStrictLimit(scan.Bytes(), &q, 8192)
		if e == nil {
			call, cancel := context.WithTimeout(ctx, 4*time.Second)
			if q.Op == "prepare-inspection" {
				v, e = r.prepareInspection(call, q.InputDigest)
			} else if q.InputDigest != "" {
				e = errors.New("input digest only allowed for preparation")
			} else {
				v, e = r.step(call, q.Op)
			}
			cancel()
		}
		if e != nil {
			v = map[string]string{"error": e.Error()}
		}
		if e = enc.Encode(v); e != nil {
			return e
		}
	}
	return scan.Err()
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

func (r *runtime) retainPublishedRecords(records []world.Component) error {
	raw, err := json.Marshal(records)
	if err != nil {
		return err
	}
	var fresh []world.Component
	if err := json.Unmarshal(raw, &fresh); err != nil {
		return err
	}
	r.publishedRecords = fresh
	return nil
}
