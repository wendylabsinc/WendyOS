// investigationfixture generates disposable loopback simulation credentials for
// native retention acceptance. It is a test tool, never an enrollment service.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"path/filepath"
	"time"
)

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func write(p string, v any) {
	b, e := json.MarshalIndent(v, "", "  ")
	must(e)
	must(os.WriteFile(p, b, 0600))
}
func read(p string, v any) { b, e := os.ReadFile(p); must(e); must(json.Unmarshal(b, v)) }
func main() {
	out := flag.String("out", "", "private output directory")
	port := flag.Int("port", 19440, "two node ports followed by two proxy ports")
	mode := flag.String("mode", "generate", "generate, upload or revoke")
	name := flag.String("node", "capture-a", "capture audience")
	file := flag.String("file", "", "upload media")
	mime := flag.String("mime", "image/png", "content type")
	ttl := flag.Int("ttl", 3600, "immutable blob seconds")
	tenant := flag.String("tenant", "lab", "fixture tenant")
	site := flag.String("site", "synthetic-office", "fixture site")
	run := flag.String("run", "investigations-replay", "historical run context")
	flag.Parse()
	if *out == "" {
		panic("explicit private output required")
	}
	abs, e := filepath.Abs(*out)
	must(e)
	must(os.MkdirAll(abs, 0700))
	must(os.Chmod(abs, 0700))
	if *mode == "upload" {
		var cfg client.Config
		read(filepath.Join(abs, *name+"-write.json"), &cfg)
		c, e := client.New(cfg)
		must(e)
		b, e := os.ReadFile(*file)
		must(e)
		v, e := c.UploadRollingEvidence(context.Background(), b, *mime, time.Now().Add(time.Duration(*ttl)*time.Second))
		must(e)
		must(json.NewEncoder(os.Stdout).Encode(v))
		return
	}
	if *mode == "wrong-authority" {
		for _, kind := range []string{"wrong-scope", "wrong-site"} {
			var cfg client.Config
			read(filepath.Join(abs, *name+"-"+kind+".json"), &cfg)
			c, e := client.New(cfg)
			must(e)
			_, e = c.PinEvidence(context.Background(), evidence.PinRequest{ID: "denied", SHA256: []string{"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}, RetainUntilUnixMS: time.Now().Add(time.Minute).UnixMilli()})
			if e == nil {
				panic("wrong authority accepted")
			}
			fmt.Printf("%s rejected\n", kind)
		}
		return
	}
	if *mode == "revoke" {
		var key []byte
		read(filepath.Join(abs, "root-private.json"), &key)
		var cfg map[string]json.RawMessage
		read(filepath.Join(abs, *name+".json"), &cfg)
		var old authority.SignedRevocations
		if _, e := os.Stat(filepath.Join(abs, *name+".revocations")); e == nil {
			read(filepath.Join(abs, *name+".revocations"), &old)
		} else {
			must(json.Unmarshal(cfg["revocations"], &old))
		}
		r := old.Revocations
		r.Revision++
		r.IssuedAtUnixMS = time.Now().UnixMilli()
		r.GrantIDs = []string{*name + "-pin"}
		signed, e := authority.SignRevocations(r, ed25519.PrivateKey(key))
		must(e)
		write(filepath.Join(abs, *name+".revocations"), signed)
		return
	}
	if *mode != "generate" {
		panic("unknown fixture mode")
	}
	if _, e := os.Stat(filepath.Join(abs, "root-private.json")); e == nil {
		panic("fixture already generated")
	}
	root, key, e := ed25519.GenerateKey(rand.Reader)
	must(e)
	write(filepath.Join(abs, "root-private.json"), []byte(key))
	now := time.Now()
	until := now.Add(59 * time.Minute)
	rev, e := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: *tenant, Site: *site, Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: until.UnixMilli()}, key)
	must(e)
	for i, n := range []string{"capture-a", "capture-b"} {
		write(filepath.Join(abs, n+".json"), map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", *port+i), "data_path": filepath.Join(abs, n+".world"), "proof_path": filepath.Join(abs, n+".proofs"), "replay_path": filepath.Join(abs, n+".replay"), "revocation_path": filepath.Join(abs, n+".revocations"), "tenant": *tenant, "site": *site, "clock_domain": "simulation:" + *run, "audience": n, "frames": []world.FrameRef{{ID: "office-floor", Revision: "office-frame-v1"}}, "root_public_key": root, "revocations": rev, "allow_insecure_simulation": true, "evidence": map[string]any{"path": filepath.Join(abs, n+".evidence"), "quota_bytes": 8 << 20, "max_blob_bytes": 8 << 20, "max_pins": 2, "max_pins_per_owner": 2}})
		for _, kind := range []string{"pin", "read", "write", "wrong-scope", "wrong-site"} {
			pub, private, e := ed25519.GenerateKey(rand.Reader)
			must(e)
			scopes := []string{"evidence-pin"}
			grantSite := *site
			switch kind {
			case "read":
				scopes = []string{"read", "evidence-read"}
			case "write":
				scopes = []string{"evidence-write"}
			case "wrong-scope":
				scopes = []string{"read"}
			case "wrong-site":
				grantSite = "other-office"
			}
			grant, e := authority.SignGrant(authority.Grant{Version: 1, ID: n + "-" + kind, PublishAudience: n, Tenant: *tenant, Site: grantSite, Domain: "simulation", Principal: "investigation-" + kind, Kind: "operator", Source: world.Source{Device: "fixture-operator", App: "investigations", Service: "retention", Stream: "main"}, PublicKey: pub, Scopes: scopes, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: until.UnixMilli()}, key)
			must(e)
			p := *port + 2 + i
			if kind == "write" {
				p = *port + i
			}
			write(filepath.Join(abs, n+"-"+kind+".json"), client.Config{URL: fmt.Sprintf("http://127.0.0.1:%d", p), Audience: n, Grant: grant, PrivateKey: private, AllowInsecureSimulation: true})
		}
	}
	fmt.Println("created disposable simulation-only loopback fixture")
}
