package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestWorlddModalityConfigRejectsBeforeState(t *testing.T) {
	for _, budgets := range []map[string]evidence.ModalityBudget{{"unknown": {}}, {"image": {QuotaBytes: -1}}, {"audio": {MaxRetentionMS: 999}}} {
		dir := t.TempDir()
		state := filepath.Join(dir, "world.json")
		raw, _ := json.Marshal(map[string]any{"allow_insecure_simulation": true, "listen": "127.0.0.1:0", "data_path": state, "tenant": "tenant", "site": "office", "audience": "node", "root_public_key": make([]byte, 32), "clock_domain": "simulation:test", "evidence": map[string]any{"modality_budgets": budgets}})
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := run(path); err == nil {
			t.Fatal("invalid modality policy accepted")
		}
		for _, suffix := range []string{"", ".evidence", ".replay", ".proofs", ".revocations"} {
			if _, err := os.Stat(state + suffix); !os.IsNotExist(err) {
				t.Fatal("invalid policy touched state", suffix, err)
			}
		}
	}
}

func TestWorlddModalityRuntime(t *testing.T) {
	if path := os.Getenv("WORLD_BUDGET_RUNTIME_CHILD"); path != "" {
		if err := run(path); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := t.TempDir()
	now := time.Now()
	root, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := authority.SignGrant(authority.Grant{Version: 1, ID: "budget-runtime", Tenant: "tenant", Site: "office", Domain: "simulation", Principal: "capture", Kind: "app", PublishAudience: "runtime", PublicKey: pub, Source: world.Source{Device: "synthetic", App: "test", Service: "capture", Stream: "bytes"}, Scopes: []string{"evidence-read", "evidence-write"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, key)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli()}, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	ln.Close()
	cfg := config{Listen: address, DataPath: filepath.Join(dir, "world.json"), Tenant: "tenant", Site: "office", ClockDomain: "simulation:runtime", Audience: "runtime", RootPublicKey: root, Revocations: rev, AllowInsecureSimulation: true, Evidence: &evidenceConfig{QuotaBytes: 1024, MaxBlobBytes: 512, ModalityBudgets: map[string]evidence.ModalityBudget{"image": {QuotaBytes: 128, MaxRetentionMS: 2000}}}}
	raw, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "config.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "child.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorlddModalityRuntime$")
	cmd.Env = append(os.Environ(), "WORLD_BUDGET_RUNTIME_CHILD="+path)
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	})
	c, err := client.New(client.Config{URL: "http://" + address, Audience: "runtime", Grant: grant, PrivateKey: private, AllowInsecureSimulation: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var usage evidence.Usage
	for ctx.Err() == nil {
		usage, err = c.EvidenceUsage(ctx)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		raw, _ := os.ReadFile(filepath.Join(dir, "child.log"))
		t.Fatalf("worldd did not serve usage: %v; child: %s", err, raw)
	}
	if usage.Modalities[2].Modality != "image" || usage.Modalities[2].QuotaBytes != 128 || usage.Modalities[2].MaxRetentionMS != 2000 {
		t.Fatal("worldd lost configured policy", usage)
	}
	before := time.Now().UnixMilli()
	p, err := c.UploadEvidence(ctx, make([]byte, 100), "image/png", time.Time{})
	if err != nil || p.RetainUntilUnixMS < before+1900 || p.RetainUntilUnixMS > time.Now().UnixMilli()+2000 {
		t.Fatal(p, err)
	}
	if _, err = c.UploadEvidence(ctx, make([]byte, 200), "image/png", time.Time{}); err == nil {
		t.Fatal("worldd ignored modality byte cap")
	}
	usage, err = c.EvidenceUsage(ctx)
	if err != nil || usage.Totals.AllocatedBytes != 100 {
		t.Fatal(usage, err)
	}
}
