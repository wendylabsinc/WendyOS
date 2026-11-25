// This private acceptance fixture publishes explicitly synthetic spatial claims.
// It is not a sensor adapter or physical hazard detector.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"time"
)

type item struct {
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
	TTLMS   int64           `json:"ttl_ms"`
}

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	credentials := flag.String("credentials", "", "private fixture publisher")
	input := flag.String("records", "", "synthetic spatial fixture JSON")
	issuer := flag.String("issuer", "", "disposable simulation issuer")
	revoke := flag.String("revoke-id", "", "fixture reader grant ID")
	flag.Parse()
	if *revoke != "" {
		raw, e := os.ReadFile(*issuer)
		if e != nil {
			return e
		}
		var saved struct {
			Domain      string                      `json:"domain"`
			PrivateKey  []byte                      `json:"private_key"`
			PublicKey   []byte                      `json:"public_key"`
			Revocations authority.SignedRevocations `json:"revocations"`
		}
		if e = authority.DecodeStrict(raw, &saved); e != nil {
			return e
		}
		if saved.Domain != "simulation" || len(saved.PrivateKey) != ed25519.PrivateKeySize {
			return fmt.Errorf("disposable simulation issuer required")
		}
		r := saved.Revocations.Revocations
		r.Revision++
		r.IssuedAtUnixMS = time.Now().Add(-time.Second).UnixMilli()
		r.GrantIDs = append(r.GrantIDs, *revoke)
		signed, e := authority.SignRevocations(r, ed25519.PrivateKey(saved.PrivateKey))
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(signed)
	}
	raw, e := os.ReadFile(*credentials)
	if e != nil {
		return e
	}
	var cfg client.Config
	if e = authority.DecodeStrict(raw, &cfg); e != nil {
		return e
	}
	if cfg.Grant.Grant.Domain != "simulation" {
		return fmt.Errorf("simulation only")
	}
	c, e := client.New(cfg)
	if e != nil {
		return e
	}
	raw, e = os.ReadFile(*input)
	if e != nil {
		return e
	}
	var items []item
	if e = authority.DecodeStrict(raw, &items); e != nil {
		return e
	}
	if len(items) == 0 || len(items) > 64 {
		return fmt.Errorf("fixture count")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	epoch, e := c.NextEpoch(ctx)
	if e != nil {
		return e
	}
	for i, item := range items {
		if item.ID == "" || item.TTLMS < 100 || item.TTLMS > 600000 {
			return fmt.Errorf("fixture bounds")
		}
		row := world.Component{Key: world.Key{Tenant: cfg.Grant.Grant.Tenant, Site: cfg.Grant.Grant.Site, Entity: item.ID, Type: "wendy.hazard.v1", Source: cfg.Grant.Grant.Source}, Version: world.Version{Epoch: epoch, Sequence: uint64(i + 1)}, Frame: world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}, Timing: world.Timing{SampleTime: time.Now(), ClockDomain: "simulation:area-workspaces", ErrorKnown: true}, TTL: time.Duration(item.TTLMS) * time.Millisecond, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "area-acceptance-fixture", Simulation: &world.SimulationContext{RunID: "area-workspaces", ScenarioVersion: "synthetic-spatial-fixtures-v1", ClockDomain: "simulation:area-workspaces"}}, Payload: item.Payload}
		if e = c.Publish(ctx, row); e != nil {
			return e
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"published": len(items), "epoch": epoch, "scope": "synthetic spatial fixtures, not physical hazard inference"})
}
