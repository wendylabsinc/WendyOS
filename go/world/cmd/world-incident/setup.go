package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/incident"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func setup(out string, contract incident.Config, base int, inspection bool) error {
	if e := contract.Validate(); e != nil {
		return e
	}
	cfg := contract.Gas
	if e := cfg.Validate(); e != nil {
		return e
	}
	if out == "" || base < 1024 || base > 65528 {
		return errors.New("fresh output and six loopback ports required")
	}
	if e := os.Mkdir(out, 0700); e != nil {
		return e
	}
	root, e := filepath.Abs(out)
	if e != nil {
		return e
	}
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return e
	}
	now := time.Now()
	until := now.Add(45 * time.Minute)
	rev, e := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: cfg.Tenant, Site: cfg.Field.Site, Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: until.UnixMilli()}, key)
	if e != nil {
		return e
	}
	write := func(name string, v any) error {
		b, e := json.MarshalIndent(v, "", "  ")
		if e != nil {
			return e
		}
		f, e := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = f.Write(append(b, '\n'))
		ce := f.Close()
		if e != nil {
			return e
		}
		return ce
	}
	credential := func(name, kind string, source world.Source, scopes []string, node int) (client.Config, error) {
		p, k, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return client.Config{}, e
		}
		audience := fmt.Sprintf("node-%d", node)
		owner := ""
		for _, scope := range scopes {
			if scope == "publish" {
				owner = audience
			}
		}
		g := authority.Grant{Version: authority.Version, PublishAudience: owner, ID: cfg.Field.Simulation.RunID + "/" + name, Tenant: cfg.Tenant, Site: cfg.Field.Site, Domain: "simulation", Principal: name, Kind: kind, Source: source, PublicKey: p, Scopes: scopes, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: until.UnixMilli()}
		if inspection && name == "node-2" {
			g.Scopes = append(g.Scopes, "task-status")
			g.TaskStatusAssignee = "atlas/navigation"
			g.TaskStatusAudience = "node-2"
		}
		grant, e := authority.SignGrant(g, key)
		return client.Config{URL: fmt.Sprintf("http://127.0.0.1:%d", base+node-1), Audience: audience, Grant: grant, PrivateKey: k, AllowInsecureSimulation: true}, e
	}
	var extras map[string]any
	if inspection {
		if e = os.Mkdir(filepath.Join(root, "inspection"), 0700); e != nil {
			return e
		}
		extras, e = inspectionSetup(root, contract, base, key, now, until, write)
		if e != nil {
			return e
		}
	}
	for n := 1; n <= 3; n++ {
		name := fmt.Sprintf("node-%d", n)
		c, e := credential(name, "device", world.Source{Device: name, App: "world", Service: "replica", Stream: "state"}, []string{"read", "replicate"}, n)
		if e != nil {
			return e
		}
		peers := []map[string]string{}
		for p := 1; p <= 3; p++ {
			if (n == 1 && p == 2) || (n == 2 && p == 1) || (n == 3 && p == 2) {
				peers = append(peers, map[string]string{"url": fmt.Sprintf("http://127.0.0.1:%d", base+3+p-1), "audience": fmt.Sprintf("node-%d", p)})
			}
		}
		nc := map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", base+n-1), "data_path": filepath.Join(root, name+".store"), "proof_path": filepath.Join(root, name+".proofs"), "replay_path": filepath.Join(root, name+".replay"), "tenant": cfg.Tenant, "site": cfg.Field.Site, "clock_domain": cfg.Field.Simulation.ClockDomain, "audience": name, "frames": []world.FrameRef{cfg.Field.Frame}, "root_public_key": pub, "revocations": rev, "peer_grant": c.Grant, "peer_private_key": c.PrivateKey, "peers": peers, "sync_interval_ms": 230 + 70*n, "allow_insecure_simulation": true, "max_records": 128}
		if inspection && n == 2 {
			for k, v := range extras {
				nc[k] = v
			}
		}
		if inspection && (n == 2 || n == 3) {
			nc["task_relay"] = map[string]any{"path": filepath.Join(root, name+".tasks"), "max_entries": 32, "max_bytes": 4 << 20, "routes": map[string]any{"node-2": map[string]string{"assignee": "atlas/navigation", "status_principal": "node-2"}}}
		}
		if e = write(name+".json", nc); e != nil {
			return e
		}
	}
	sensors := map[string]client.Config{}
	for i, s := range cfg.Field.Sensors {
		node := 1 + i%2
		c, e := credential("sensor-"+s.ID, "app", s.Source, []string{"publish"}, node)
		if e != nil {
			return e
		}
		sensors[s.ID] = c
	}
	if e = write("sensors.json", gasRuntimeConfig{Public: cfg, Sensors: sensors}); e != nil {
		return e
	}
	for i, name := range []string{"east", "west"} {
		source, ok := cfg.Gateways[name]
		if !ok {
			return errors.New("lab gateway names must be east and west")
		}
		c, e := credential("estimator-"+name, "app", source, []string{"read", "publish"}, i+1)
		if e != nil {
			return e
		}
		if e = write(name+".json", gasRuntimeConfig{Public: cfg, Gateway: name, Client: &c}); e != nil {
			return e
		}
		reader, e := credential("operator-"+name, "operator", world.Source{Device: "operator-" + name, App: "gas-view", Service: "reader", Stream: "state"}, []string{"read"}, i+2)
		if e != nil {
			return e
		}
		if e = write("viewer-"+name+".json", gasRuntimeConfig{Public: cfg, Reader: name, Client: &reader}); e != nil {
			return e
		}
	}
	revoked := rev.Revocations
	revoked.Revision = 2
	revoked.GrantIDs = []string{cfg.Field.Simulation.RunID + "/sensor-" + cfg.Field.Sensors[0].ID}
	signedRevoked, e := authority.SignRevocations(revoked, key)
	if e != nil {
		return e
	}
	if e = write("revoke-first-sensor.json", signedRevoked); e != nil {
		return e
	}
	if e = write("public.json", cfg); e != nil {
		return e
	}
	visual, e := credential("visual-adapter", "app", contract.VisualPublisher, []string{"read", "publish"}, 1)
	if e != nil {
		return e
	}
	if e = write("incident-visual.json", runtimeConfig{Public: contract, Client: visual}); e != nil {
		return e
	}
	for i, name := range []string{"east", "west"} {
		reader, e := credential("incident-"+name, "app", contract.Readers[name], []string{"read", "publish"}, i+2)
		if e != nil {
			return e
		}
		if e = write("incident-"+name+".json", runtimeConfig{Public: contract, Reader: name, Client: reader}); e != nil {
			return e
		}
	}
	return write("incident-public.json", contract)
}
