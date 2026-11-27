// world-gas-replica runs independent simulation publishers, estimators and
// readers against ordinary authenticated World nodes. It starts no nodes.
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/gasview"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type runtimeConfig struct {
	Public  gasreplica.Config        `json:"public"`
	Gateway string                   `json:"gateway,omitempty"`
	Reader  string                   `json:"reader,omitempty"`
	Client  *client.Config           `json:"client,omitempty"`
	Sensors map[string]client.Config `json:"sensors,omitempty"`
}
type request struct {
	Op           string                 `json:"op"`
	Observations []gasfield.Observation `json:"observations,omitempty"`
	Component    *world.Component       `json:"component,omitempty"`
}
type signedPending struct {
	body      []byte
	component world.Component
}
type publisher struct {
	cfg        client.Config
	api        *client.Client
	epoch, seq uint64
	pending    *signedPending
	last       *signedPending
}

func newPublisher(cfg client.Config) (*publisher, error) {
	c, e := client.New(cfg)
	return &publisher{cfg: cfg, api: c}, e
}
func (p *publisher) version(ctx context.Context) (world.Version, error) {
	if p.epoch == 0 {
		epoch, e := p.api.NextEpoch(ctx)
		if e != nil {
			return world.Version{}, e
		}
		p.epoch = epoch
	}
	p.seq++
	return world.Version{Epoch: p.epoch, Sequence: p.seq}, nil
}
func (p *publisher) prepare(c world.Component) (*signedPending, error) {
	signed, e := authority.SignRecord(p.cfg.Grant, ed25519.PrivateKey(p.cfg.PrivateKey), c, time.Now())
	if e != nil {
		return nil, e
	}
	if e = gasreplica.CheckSignedSize(signed); e != nil {
		return nil, e
	}
	raw, e := json.Marshal(signed)
	return &signedPending{raw, c}, e
}
func (p *publisher) send(ctx context.Context, pending *signedPending) error {
	return p.api.Do(ctx, "publish", "POST", "/v1/components", pending.body, nil)
}
func (p *publisher) retry(ctx context.Context) error {
	if p.pending == nil {
		return nil
	}
	if gasreplica.Remaining(p.pending.component, time.Now()) <= 0 {
		p.pending = nil
		return errors.New("original publication expired before retry")
	}
	if e := p.send(ctx, p.pending); e != nil {
		return e
	}
	p.last = p.pending
	p.pending = nil
	return nil
}

type runtime struct {
	cfg       runtimeConfig
	role      string
	api       *client.Client
	publisher *publisher
	sensors   map[string]*publisher
	view      *gasview.Selector
	lastInput string
	possible  *world.Component // includes a POST whose acknowledgement was lost
}

// A shorter replacement cannot erase the withdrawal obligation of an earlier
// POST whose acceptance is uncertain. The next tombstone covers the longest
// original lifetime without making it longer.
func (r *runtime) rememberPossible(c world.Component) {
	if r.possible == nil || c.Timing.SampleTime.Add(c.TTL).After(r.possible.Timing.SampleTime.Add(r.possible.TTL)) {
		r.possible = &c
	}
}

func newRuntime(cfg runtimeConfig, role string) (*runtime, error) {
	if e := cfg.Public.Validate(); e != nil {
		return nil, e
	}
	r := &runtime{cfg: cfg, role: role, sensors: map[string]*publisher{}}
	var e error
	switch role {
	case "sensors":
		if len(cfg.Sensors) != len(cfg.Public.Field.Sensors) {
			return nil, errors.New("exact sensor credential catalog required")
		}
		for _, s := range cfg.Public.Field.Sensors {
			c, ok := cfg.Sensors[s.ID]
			if !ok || c.Grant.Grant.Source != s.Source {
				return nil, errors.New("sensor grant differs from catalog")
			}
			p, e := newPublisher(c)
			if e != nil {
				return nil, e
			}
			r.sensors[s.ID] = p
		}
	case "estimator", "reader", "probe":
		if cfg.Client == nil {
			return nil, errors.New("one independent credential required")
		}
		r.api, e = client.New(*cfg.Client)
		if e != nil {
			return nil, e
		}
		if role == "estimator" {
			source, ok := cfg.Public.Gateways[cfg.Gateway]
			if !ok || source != cfg.Client.Grant.Grant.Source {
				return nil, errors.New("gateway source differs from its grant")
			}
			r.publisher, e = newPublisher(*cfg.Client)
		}
		if role == "reader" {
			r.view, e = gasview.New(cfg.Public)
		}
	default:
		return nil, errors.New("role must be sensors, estimator, reader or probe")
	}
	return r, e
}
func (r *runtime) step(ctx context.Context, q request) (any, error) {
	if q.Op == "status" {
		return map[string]any{"role": r.role, "contract_sha256": r.cfg.Public.ContractDigest()}, nil
	}
	if r.role == "probe" {
		if q.Op == "epoch" {
			epoch, e := r.api.NextEpoch(ctx)
			return map[string]any{"epoch": epoch}, e
		}
		if q.Op == "query" {
			return r.api.Query(ctx)
		}
		if q.Op != "publish" || q.Component == nil {
			return nil, errors.New("probe requires component or query")
		}
		p, e := newPublisher(*r.cfg.Client)
		if e != nil {
			return nil, e
		}
		signed, e := p.prepare(*q.Component)
		if e != nil {
			return nil, e
		}
		return map[string]any{"sha256": gasreplica.ComponentHash(*q.Component)}, p.send(ctx, signed)
	}
	if r.role == "sensors" {
		if q.Op == "duplicates" {
			count := 0
			for _, p := range r.sensors {
				if p.last != nil {
					if e := p.send(ctx, p.last); e != nil {
						return nil, e
					}
					count++
				}
			}
			return map[string]any{"resent_originals": count}, nil
		}
		if q.Op != "publish" || len(q.Observations) > 16 {
			return nil, errors.New("publish requires at most sixteen sensor originals")
		}
		// Validate all inputs before any publication. Epochs are assigned only by
		// the authoritative owner node; callers cannot choose restart identities.
		components := []world.Component{}
		seen := map[string]bool{}
		for _, o := range q.Observations {
			if seen[o.Sensor] {
				return nil, errors.New("duplicate sensor in batch")
			}
			seen[o.Sensor] = true
			c, e := gasreplica.ObservationComponent(r.cfg.Public, o)
			if e != nil {
				return nil, e
			}
			if gasreplica.Remaining(c, time.Now()) <= 0 || c.Timing.SampleTime.After(time.Now()) {
				return nil, errors.New("original capture is not current")
			}
			components = append(components, c)
		}
		receipts := make([]map[string]any, len(q.Observations))
		errs := make([]error, len(q.Observations))
		var wg sync.WaitGroup
		for i, o := range q.Observations {
			wg.Add(1)
			go func(i int, o gasfield.Observation) {
				defer wg.Done()
				p := r.sensors[o.Sensor]
				if e := p.retry(ctx); e != nil {
					errs[i] = e
					return
				}
				version, e := p.version(ctx)
				if e != nil {
					errs[i] = e
					return
				}
				o.Version = version
				c, e := gasreplica.ObservationComponent(r.cfg.Public, o)
				if e != nil {
					errs[i] = e
					return
				}
				pending, e := p.prepare(c)
				if e != nil {
					errs[i] = e
					return
				}
				p.pending = pending
				if e = p.retry(ctx); e != nil {
					errs[i] = e
					return
				}
				receipts[i] = map[string]any{"sensor": o.Sensor, "version": version, "sha256": gasreplica.ComponentHash(c), "signed_bytes": len(pending.body), "capture": components[i].Timing.SampleTime}
			}(i, o)
		}
		wg.Wait()
		for _, e := range errs {
			if e != nil {
				return nil, e
			}
		}

		return map[string]any{"published": receipts}, nil
	}
	if q.Op != "tick" {
		return nil, errors.New("reader and estimator accept tick")
	}
	snapshot, e := r.api.Query(ctx)
	if e != nil {
		return nil, e
	}
	now := time.Now()
	if r.role == "reader" {
		selection, e := r.view.Step(snapshot.Components, now)
		return map[string]any{"reader": r.cfg.Reader, "node": r.cfg.Client.Audience, "selection": selection, "received_at": now}, e
	}
	p := r.publisher
	if e = p.retry(ctx); e != nil {
		return nil, e
	}
	result, inputs, heldout, e := gasreplica.Evaluate(r.cfg.Public, snapshot.Components, now)
	if e != nil {
		return nil, e
	}
	response := map[string]any{"gateway": r.cfg.Gateway, "result": result, "held_out": heldout, "published": false}
	if result.Estimate == nil {
		r.lastInput = ""
		if r.possible != nil && gasreplica.Remaining(*r.possible, now) > 0 {
			tomb := *r.possible
			version, e := p.version(ctx)
			if e != nil {
				return nil, e
			}
			tomb.Version = version
			tomb.Tombstone = true
			tomb.Payload = nil
			pending, e := p.prepare(tomb)
			if e != nil {
				return nil, e
			}
			p.pending = pending
			if e = p.retry(ctx); e != nil {
				return nil, e
			}
			response["withdrawn"] = true
		}
		r.possible = nil
		return response, nil
	}
	hashes := []string{}
	for _, c := range inputs {
		hashes = append(hashes, gasreplica.ComponentHash(c))
	}
	digest := gasreplica.Digest(hashes)
	if digest == r.lastInput {
		return response, nil
	}
	version, e := p.version(ctx)
	if e != nil {
		return nil, e
	}
	component, e := gasreplica.MakeClaim(r.cfg.Public, r.cfg.Gateway, version, result, inputs)
	if e != nil {
		return nil, e
	}
	pending, e := p.prepare(component)
	if e != nil {
		return nil, e
	}
	p.pending = pending
	r.rememberPossible(component)
	if e = p.retry(ctx); e != nil {
		return nil, e
	}
	r.lastInput = digest
	response["published"] = true
	response["component"] = component
	response["sha256"] = gasreplica.ComponentHash(component)
	response["signed_bytes"] = len(pending.body)
	return response, nil
}
func load(path string, out any, limit int64) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return e
	}
	if int64(len(raw)) > limit {
		return errors.New("configuration exceeds bound")
	}
	return authority.DecodeStrictLimit(raw, out, int(limit))
}
func run() error {
	role := flag.String("role", "", "setup, sensors, estimator, reader or probe (simulation diagnostics)")
	path := flag.String("config", "", "private configuration file")
	out := flag.String("out", "", "fresh private setup directory")
	base := flag.Int("base-port", 19280, "three node ports followed by three fault-proxy ports")
	flag.Parse()
	if *role == "setup" {
		var cfg gasreplica.Config
		if e := load(*path, &cfg, 128<<10); e != nil {
			return e
		}
		return setup(*out, cfg, *base)
	}
	var cfg runtimeConfig
	if e := load(*path, &cfg, 256<<10); e != nil {
		return e
	}
	r, e := newRuntime(cfg, *role)
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Close stdin on cancellation so the idle stream also terminates.
	go func() { <-ctx.Done(); _ = os.Stdin.Close() }()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 128<<10)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		var q request
		var value any
		e = authority.DecodeStrictLimit(scanner.Bytes(), &q, 128<<10)
		if e == nil {
			call, cancel := context.WithTimeout(ctx, 4*time.Second)
			value, e = r.step(call, q)
			cancel()
		}
		if e != nil {
			value = map[string]any{"error": e.Error()}
		}
		if e = encoder.Encode(value); e != nil {
			return e
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return scanner.Err()
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
