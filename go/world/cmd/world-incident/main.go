// world-incident associates simulation evidence using ordinary World clients.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/incident"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type gasRuntimeConfig struct {
	Public  gasreplica.Config        `json:"public"`
	Gateway string                   `json:"gateway,omitempty"`
	Reader  string                   `json:"reader,omitempty"`
	Client  *client.Config           `json:"client,omitempty"`
	Sensors map[string]client.Config `json:"sensors,omitempty"`
}
type runtimeConfig struct {
	Public incident.Config `json:"public"`
	Reader string          `json:"reader,omitempty"`
	Client client.Config   `json:"client"`
}
type request struct {
	InputDigest string            `json:"input_digest,omitempty"`
	Op          string            `json:"op"`
	Samples     []sensing.Sample  `json:"samples,omitempty"`
	Images      map[string][]byte `json:"images,omitempty"`
	TTLMS       int64             `json:"ttl_ms,omitempty"`
}
type runtime struct {
	cfg             runtimeConfig
	role            string
	p               *publisher
	view            *incident.Selector
	lastInput       string
	possible        *world.Component
	lastResult      *incident.Result
	publishedResult *incident.Result
}

func newRuntime(cfg runtimeConfig, role string) (*runtime, error) {
	if e := cfg.Public.Validate(); e != nil {
		return nil, e
	}
	expected := cfg.Public.VisualPublisher
	if role == "reader" {
		var ok bool
		expected, ok = cfg.Public.Readers[cfg.Reader]
		if !ok {
			return nil, errors.New("configured independent reader required")
		}
	} else if role != "visual" {
		return nil, errors.New("role must be visual or reader")
	}
	if cfg.Client.Grant.Grant.Source != expected {
		return nil, errors.New("grant source differs from configured application")
	}
	p, e := newPublisher(cfg.Client)
	if e != nil {
		return nil, e
	}
	view, e := incident.New(cfg.Public)
	return &runtime{cfg: cfg, role: role, p: p, view: view}, e
}
func (r *runtime) withdraw(ctx context.Context) error {
	if r.possible == nil {
		return nil
	}
	old := *r.possible
	if gasreplica.Remaining(old, time.Now()) <= 0 {
		r.possible = nil
		return nil
	}
	v, e := r.p.version(ctx)
	if e != nil {
		return e
	}
	old.Version = v
	old.Tombstone = true
	old.Payload = nil
	pending, e := r.p.prepare(old)
	if e != nil {
		return e
	}
	r.p.pending = pending
	if e = r.p.retry(ctx); e != nil {
		return e
	}
	r.possible = nil
	r.lastInput = ""
	return nil
}
func (r *runtime) publish(ctx context.Context, c world.Component, digest string) (map[string]any, error) {
	if gasreplica.Remaining(c, time.Now()) <= 0 {
		return nil, errors.New("support expired before publish")
	}
	pending, e := r.p.prepare(c)
	if e != nil {
		return nil, e
	}
	r.p.pending = pending
	if r.possible == nil || c.Timing.SampleTime.Add(c.TTL).After(r.possible.Timing.SampleTime.Add(r.possible.TTL)) {
		copy := c
		r.possible = &copy
	}
	if e = r.p.retry(ctx); e != nil {
		return nil, e
	}
	r.lastInput = digest
	return map[string]any{"published": true, "sha256": gasreplica.ComponentHash(c), "signed_bytes": len(pending.body), "component": c}, nil
}
func (r *runtime) step(ctx context.Context, q request) (any, error) {
	if q.Op == "prepare-inspection" {
		if r.role != "reader" || r.lastResult == nil || q.InputDigest == "" || q.InputDigest != r.lastResult.InputDigest || r.p.pending != nil || r.p.last == nil || r.publishedResult == nil || r.publishedResult.InputDigest != q.InputDigest {
			return nil, errors.New("select an unchanged current incident before preparing inspection")
		}
		now := time.Now()
		if !now.Before(r.lastResult.ValidUntil) {
			return nil, errors.New("current selection expired")
		}
		c, e := incident.InspectionSelection(r.cfg.Public, r.cfg.Reader, r.p.last.component.Version, *r.publishedResult, now)
		if e != nil {
			return nil, e
		}
		selected, e := missioncontrol.DecodeIncidentSelection(c)
		if e != nil || selected.IncidentSHA256 != gasreplica.ComponentHash(r.p.last.component) {
			return nil, errors.New("selected incident publication changed")
		}
		trigger, e := tasking.TriggerFromComponent(c)
		if e != nil {
			return nil, e
		}
		raw, _ := json.Marshal(trigger)
		hash := sha256.Sum256(r.p.last.body)
		return map[string]any{"version": 1, "trigger_json": raw, "trigger_sha256": trigger.SHA256, "target": selected.Position, "envelope_sha256": hex.EncodeToString(hash[:]), "result_schema": missioncontrol.IncidentInspectionType, "sample_valid_until": selected.ValidUntil, "physical_valid_until": selected.ValidUntil}, nil
	}

	if q.Op == "duplicates" {
		if r.p.last == nil {
			return nil, errors.New("no prior original")
		}
		return map[string]bool{"resent_original": true}, r.p.send(ctx, r.p.last)
	}
	if q.Op == "query" {
		return r.p.api.Query(ctx)
	}
	if e := r.p.retry(ctx); e != nil {
		return nil, e
	}
	if r.role == "reader" {
		if q.Op != "tick" {
			return nil, errors.New("reader expects tick")
		}
		snapshot, e := r.p.api.Query(ctx)
		if e != nil {
			return nil, e
		}
		result, e := r.view.Step(snapshot.Components, time.Now())
		if e != nil {
			return nil, e
		}
		response := map[string]any{"reader": r.cfg.Reader, "node": r.cfg.Client.Audience, "result": result, "published": false}
		if len(result.Supports) == 0 {
			e = r.withdraw(ctx)
		} else if result.InputDigest != r.lastInput {
			v, err := r.p.version(ctx)
			if err != nil {
				return nil, err
			}
			c, err := incident.Component(r.cfg.Public, r.cfg.Reader, v, result)
			if err != nil {
				return nil, err
			}
			receipt, err := r.publish(ctx, c, result.InputDigest)
			if err != nil {
				return nil, err
			}
			// Retain the exact evaluation used by the signed publication. A later
			// reevaluation may reduce local deadlines without changing input identity.
			r.publishedResult = &result
			response["publication"] = receipt
			response["published"] = true
		}
		if e == nil {
			r.lastResult = &result
		}
		return response, e
	}
	if q.Op == "withdraw" {
		return map[string]bool{"withdrawn": true}, r.withdraw(ctx)
	}
	if q.Op != "capture" || len(q.Samples) != 3 || q.TTLMS < 1 || q.TTLMS > 10000 {
		return nil, errors.New("capture requires three originals and TTL in1..10000ms")
	}
	// Original sequence/timing comes from the adapter, never receipt or repair.
	records := []world.Component{}
	for _, sample := range q.Samples {
		cfg := r.cfg.Public.Visual
		source, ok := cfg.Sources[sample.SensorID]
		if !ok {
			return nil, errors.New("unknown visual sensor")
		}
		converter, e := sensing.New(sensing.Config{Tenant: cfg.Tenant, Site: cfg.Site, RunID: cfg.Simulation.RunID, Frame: cfg.Frame, Source: source, Epoch: 1, Anchor: cfg.Anchor, TTL: time.Duration(q.TTLMS) * time.Millisecond})
		if e != nil {
			return nil, e
		}
		raw, e := json.Marshal(sample)
		if e != nil {
			return nil, e
		}
		c, e := converter.Convert(raw)
		if e != nil {
			return nil, e
		}
		records = append(records, c)
	}
	digest := gasreplica.Digest(records)
	// A validation still runs for unchanged support; it cannot keep a stale output.
	v := world.Version{Epoch: 1, Sequence: 1}
	c, e := incident.MakeVisual(r.cfg.Public, v, records, q.Images, time.Now())
	if e != nil {
		if err := r.withdraw(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"state": "unavailable", "reason": e.Error(), "withdrawn": true}, nil
	}
	if digest == r.lastInput {
		return map[string]any{"state": "localized", "published": false}, nil
	}
	v, e = r.p.version(ctx)
	if e != nil {
		return nil, e
	}
	c.Version = v
	receipt, e := r.publish(ctx, c, digest)
	if receipt != nil {
		receipt["state"] = "localized"
	}
	return receipt, e
}
func load(path string, out any) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 256<<10+1))
	if e != nil {
		return e
	}
	return authority.DecodeStrictLimit(b, out, 256<<10)
}
func run() error {
	inspectionMode := flag.Bool("inspection", false, "commission optional simulation inspection authority")
	role := flag.String("role", "", "setup,visual,reader")
	path := flag.String("config", "", "private config")
	out := flag.String("out", "", "fresh setup directory")
	base := flag.Int("base-port", 19480, "six loopback ports")
	flag.Parse()
	if *role == "setup" {
		var cfg incident.Config
		if e := load(*path, &cfg); e != nil {
			return e
		}
		return setup(*out, cfg, *base, *inspectionMode)
	}
	var cfg runtimeConfig
	if e := load(*path, &cfg); e != nil {
		return e
	}
	r, e := newRuntime(cfg, *role)
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); _ = os.Stdin.Close() }()
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 64<<10), 256<<10)
	enc := json.NewEncoder(os.Stdout)
	for scan.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		var q request
		var value any
		e = authority.DecodeStrictLimit(scan.Bytes(), &q, 256<<10)
		if e == nil {
			call, cancel := context.WithTimeout(ctx, 4*time.Second)
			value, e = r.step(call, q)
			cancel()
		}
		if e != nil {
			value = map[string]string{"error": e.Error()}
		}
		if e = enc.Encode(value); e != nil {
			return e
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return scan.Err()
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
