package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/incidentchange"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"path/filepath"
	"time"
)

func setup(out string, c incidentchange.Config, base int) error {
	return setupOptions(out, c, base, false)
}
func setupOptions(out string, c incidentchange.Config, base int, inspection bool) error {
	if e := c.Validate(); e != nil {
		return e
	}
	if out == "" || base < 1024 || base > 65528 {
		return errors.New("fresh private directory and six loopback ports required")
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
	cfg := c.Spatial.Gas
	rev, e := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: cfg.Tenant, Site: cfg.Field.Site, Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: until.UnixMilli()}, key)
	if e != nil {
		return e
	}
	write := func(name string, v any) error {
		raw, e := json.MarshalIndent(v, "", "  ")
		if e != nil {
			return e
		}
		f, e := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = f.Write(append(raw, '\n'))
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
		aud := fmt.Sprintf("node-%d", node)
		owner := ""
		for _, s := range scopes {
			if s == "publish" {
				owner = aud
			}
		}
		g := authority.Grant{Version: authority.Version, ID: cfg.Field.Simulation.RunID + "/" + name, Tenant: cfg.Tenant, Site: cfg.Field.Site, Domain: "simulation", Principal: name, Kind: kind, Source: source, PublicKey: p, PublishAudience: owner, Scopes: scopes, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: until.UnixMilli()}
		if inspection && name == "node-2" {
			g.Scopes = append(g.Scopes, "task-status")
			g.TaskStatusAssignee = "atlas/navigation"
			g.TaskStatusAudience = "node-2"
		}
		grant, e := authority.SignGrant(g, key)
		return client.Config{URL: fmt.Sprintf("http://127.0.0.1:%d", base+node-1), Audience: aud, Grant: grant, PrivateKey: k, AllowInsecureSimulation: true}, e
	}
	trustPath := filepath.Join(root, "member-trust.json")
	if e = write("member-trust.json", pairTrust{pub, rev}); e != nil {
		return e
	}
	var extras map[string]any
	if inspection {
		if e = os.Mkdir(filepath.Join(root, "inspection"), 0700); e != nil {
			return e
		}
		extras, e = inspectionSetup(root, c.Spatial, base, key, now, until, write)
		if e != nil {
			return e
		}
	}
	for n := 1; n <= 3; n++ {
		name := fmt.Sprintf("node-%d", n)
		cc, e := credential(name, "device", world.Source{Device: name, App: "world", Service: "replica", Stream: "state"}, []string{"read", "replicate"}, n)
		if e != nil {
			return e
		}
		peers := []map[string]string{}
		if n == 3 {
			peers = append(peers, map[string]string{"url": fmt.Sprintf("http://127.0.0.1:%d", base+3), "audience": "node-1"})
		}
		if n > 1 {
			peers = append(peers, map[string]string{"url": fmt.Sprintf("http://127.0.0.1:%d", base+3+n-2), "audience": fmt.Sprintf("node-%d", n-1)})
		}
		nc := map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", base+n-1), "data_path": filepath.Join(root, name+".store"), "proof_path": filepath.Join(root, name+".proofs"), "replay_path": filepath.Join(root, name+".replay"), "tenant": cfg.Tenant, "site": cfg.Field.Site, "clock_domain": cfg.Field.Simulation.ClockDomain, "audience": name, "frames": []world.FrameRef{cfg.Field.Frame}, "root_public_key": pub, "revocations": rev, "peer_grant": cc.Grant, "peer_private_key": cc.PrivateKey, "peers": peers, "sync_interval_ms": 300 + 25*n, "allow_insecure_simulation": true, "max_records": 128, "diagnostics": map[string]string{"path": filepath.Join(root, name+".metrics")}}
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
	for _, s := range cfg.Field.Sensors {
		cc, e := credential("sensor-"+s.ID, "app", s.Source, []string{"publish"}, 1)
		if e != nil {
			return e
		}
		sensors[s.ID] = cc
	}
	if e = write("sensors.json", map[string]any{"public": cfg, "sensors": sensors}); e != nil {
		return e
	}
	visual, e := credential("visual-adapter", "app", c.Spatial.VisualPublisher, []string{"read", "publish"}, 1)
	if e != nil {
		return e
	}
	if e = write("incident-visual.json", map[string]any{"public": c.Spatial, "client": visual}); e != nil {
		return e
	}
	for i, name := range []string{"east", "west"} {
		source, ok := cfg.Gateways[name]
		if !ok {
			return errors.New("east and west gateway catalog required")
		}
		cc, e := credential("estimator-"+name, "app", source, []string{"read", "publish"}, i+1)
		if e != nil {
			return e
		}
		if e = write("estimate-"+name+".json", runtimeConfig{Public: c, Gateway: name, Client: cc, TrustPath: trustPath, Watch: true}); e != nil {
			return e
		}
		if e = write(name+".json", map[string]any{"public": cfg, "gateway": name, "client": cc}); e != nil {
			return e
		}
		if e = write("trend-"+name+".json", map[string]any{"public": cfg, "policy": c.Temporal, "gateway": name, "client": cc}); e != nil {
			return e
		}
		reader, e := credential("incident-"+name, "app", c.Spatial.Readers[name], []string{"read", "publish"}, i+2)
		if e != nil {
			return e
		}
		if e = write("incident-"+name+".json", map[string]any{"public": c.Spatial, "reader": name, "client": reader}); e != nil {
			return e
		}
		if e = write("change-"+name+".json", runtimeConfig{Public: c, Reader: name, Client: reader, TrustPath: trustPath, Watch: true}); e != nil {
			return e
		}
	}
	return write("public.json", c)
}
