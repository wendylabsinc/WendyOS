package main

import (
	"context"
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
	"github.com/wendylabsinc/wendy/go/world/internal/patrolreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// setup is an explicit, short-lived local simulation issuer. Its keys stay in
// the private output directory and are never used by the executor or its signer.
func setup(ctx context.Context, out, address, captures string, base int) error {
	if base < 1024 || base > 65529 || !filepath.IsAbs(captures) {
		return errors.New("six loopback ports and absolute capture path required")
	}
	b, e := status(ctx, address)
	if e != nil {
		return e
	}
	var source patrolreplica.Snapshot
	if e = json.Unmarshal(b, &source); e != nil {
		return e
	}
	now := time.Now()
	until := now.Add(45 * time.Minute)
	cfg := patrolreplica.Config{Tenant: "simulation", Site: "studio-01", RunID: source.RunID, MissionID: source.Patrol.MissionID, SpecSHA256: patrolreplica.JSONHash(source.Task.Spec), Frame: source.Task.Spec.Frame, Source: world.Source{Device: "go2", App: "world-patrol-replica", Service: "debrief", Stream: "reports"}, RetainUntilMS: until.UnixMilli()}
	if e = cfg.Validate(); e != nil {
		return e
	}
	if _, _, e = patrolreplica.DecodeSnapshot(b, cfg, now); e != nil {
		return e
	}
	if e = os.Mkdir(out, 0700); e != nil {
		return e
	}
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return e
	}
	rev, e := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: cfg.Tenant, Site: cfg.Site, Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: until.UnixMilli()}, key)
	if e != nil {
		return e
	}
	credential := func(name, kind string, source world.Source, scopes []string, node int) (client.Config, error) {
		p, k, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return client.Config{}, e
		}
		audience := fmt.Sprintf("patrol-node-%d", node)
		owner := ""
		for _, scope := range scopes {
			if scope == "publish" || scope == "evidence-write" {
				owner = audience
			}
		}
		grant, e := authority.SignGrant(authority.Grant{Version: authority.Version, PublishAudience: owner, ID: cfg.RunID + "/debrief/" + name, Tenant: cfg.Tenant, Site: cfg.Site, Domain: "simulation", Principal: name, Kind: kind, Source: source, PublicKey: p, Scopes: scopes, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: until.UnixMilli()}, key)
		return client.Config{URL: fmt.Sprintf("http://127.0.0.1:%d", base+node-1), Audience: audience, Grant: grant, PrivateKey: k, AllowInsecureSimulation: true}, e
	}
	write := func(name string, v any) error { return patrolreplica.AtomicJSON(filepath.Join(out, name), v) }
	for n := 1; n <= 3; n++ {
		name := fmt.Sprintf("patrol-node-%d", n)
		c, e := credential(name, "device", world.Source{Device: name, App: "world", Service: "replica", Stream: "state"}, []string{"read", "replicate"}, n)
		if e != nil {
			return e
		}
		peers := []map[string]string{}
		for p := 1; p <= 3; p++ {
			if n == 1 && p == 2 || n == 2 && p == 1 || n == 3 && p == 2 {
				peers = append(peers, map[string]string{"url": fmt.Sprintf("http://127.0.0.1:%d", base+3+p-1), "audience": fmt.Sprintf("patrol-node-%d", p)})
			}
		}
		nc := map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", base+n-1), "data_path": filepath.Join(out, name+".store"), "proof_path": filepath.Join(out, name+".proofs"), "replay_path": filepath.Join(out, name+".replay"), "tenant": cfg.Tenant, "site": cfg.Site, "clock_domain": cfg.Clock(), "audience": name, "frames": []world.FrameRef{cfg.Frame}, "root_public_key": pub, "revocations": rev, "peer_grant": c.Grant, "peer_private_key": c.PrivateKey, "peers": peers, "sync_interval_ms": 300 + 70*n, "allow_insecure_simulation": true, "max_records": 128}
		if n == 1 {
			nc["evidence"] = map[string]any{"path": filepath.Join(out, "originals"), "quota_bytes": 256 << 20, "max_blob_bytes": 16 << 20, "max_retention_ms": int64((45 * time.Minute) / time.Millisecond), "max_entries": 256}
		}
		if e = write(name+".json", nc); e != nil {
			return e
		}
	}
	publisher, e := credential("patrol-publisher", "app", cfg.Source, []string{"read", "publish", "evidence-write"}, 1)
	if e != nil {
		return e
	}
	if e = write("publisher.json", config{Public: cfg, Client: publisher, StatusURL: address, CaptureDir: captures}); e != nil {
		return e
	}
	for i, name := range []string{"east", "west"} {
		reader, e := credential("operator-"+name, "operator", world.Source{Device: "operator-" + name, App: "patrol-debrief", Service: "reader", Stream: "state"}, []string{"read", "evidence-read"}, i+2)
		if e != nil {
			return e
		}
		originals := reader
		originals.URL = fmt.Sprintf("http://127.0.0.1:%d", base+3)
		originals.Audience = "patrol-node-1"
		if e = write("reader-"+name+".json", config{Public: cfg, Client: reader, EvidenceClient: &originals, Reader: name}); e != nil {
			return e
		}
	}
	return write("public.json", cfg)
}
