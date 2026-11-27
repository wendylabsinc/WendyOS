//go:build !world_wendy_boot

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func bootstrapFixture(t *testing.T) (string, string) {
	t.Helper()
	opts := options(t)
	if _, err := stage(opts, fakeBuild); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(opts.Out, "project", "node-seed.json"), filepath.Join(t.TempDir(), "data")
}

func TestBootstrapPersistsAndReusesExactScope(t *testing.T) {
	seed, dir := bootstrapFixture(t)
	path, err := prepareNode(seed, dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "node.json") {
		t.Fatal(path)
	}
	if err = os.WriteFile(filepath.Join(dir, "world.state"), []byte("retained checkpoint"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = prepareNode(seed, dir); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if !bytes.Equal(first, second) {
		t.Fatal("restart replaced config")
	}
	state, _ := os.ReadFile(filepath.Join(dir, "world.state"))
	if string(state) != "retained checkpoint" {
		t.Fatal("restart replaced state")
	}
	var cfg seedConfig
	decodeFile(t, seed, &cfg)
	cfg.ClockDomain = "simulation:new-run"
	changed, _ := json.Marshal(cfg)
	if err = os.WriteFile(seed, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = prepareNode(seed, dir); err == nil {
		t.Fatal("new run reused old volume")
	}
	retained, _ := os.ReadFile(path)
	if !bytes.Equal(first, retained) {
		t.Fatal("rejected seed changed durable config")
	}
}

func TestBootstrapRejectsDamagedStateUnsafeSeedsAndSymlinks(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"tasks":          func(c map[string]any) { c["tasks"] = map[string]any{"catalog": map[string]string{"move": "MOVE"}} },
		"private key":    func(c map[string]any) { c["peer_private_key"] = "not-allowed" },
		"peers":          func(c map[string]any) { c["peers"] = []string{"http://other"} },
		"physical clock": func(c map[string]any) { c["clock_domain"] = "physical:utc" },
		"wrong state":    func(c map[string]any) { c["data_path"] = "/etc/state" },
		"missing opt-in": func(c map[string]any) { c["allow_insecure_simulation"] = false },
	} {
		t.Run(name, func(t *testing.T) {
			seed, dir := bootstrapFixture(t)
			var cfg map[string]any
			decodeFile(t, seed, &cfg)
			edit(cfg)
			b, _ := json.Marshal(cfg)
			if err := os.WriteFile(seed, b, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := prepareNode(seed, dir); err == nil {
				t.Fatal("unsafe seed accepted")
			}
		})
	}
	t.Run("truncated stored configuration", func(t *testing.T) {
		seed, dir := bootstrapFixture(t)
		path, err := prepareNode(seed, dir)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, []byte(`{"listen":`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = prepareNode(seed, dir); err == nil {
			t.Fatal("damaged config silently reinitialized")
		}
	})
	t.Run("symlink volume", func(t *testing.T) {
		seed, dir := bootstrapFixture(t)
		if err := os.Symlink(t.TempDir(), dir); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareNode(seed, dir); err == nil {
			t.Fatal("symlink volume accepted")
		}
	})
	t.Run("symlink stored configuration", func(t *testing.T) {
		seed, dir := bootstrapFixture(t)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(seed, filepath.Join(dir, "node.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareNode(seed, dir); err == nil {
			t.Fatal("symlink config accepted")
		}
	})
	t.Run("symlink bootstrap lock", func(t *testing.T) {
		seed, dir := bootstrapFixture(t)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(seed, filepath.Join(dir, "bootstrap.lock")); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareNode(seed, dir); err == nil {
			t.Fatal("root bootstrap followed lock symlink")
		}
	})
}

func TestBootstrapRecoversInterruptedTemporaryWrite(t *testing.T) {
	seed, dir := bootstrapFixture(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".node-seed-interrupted"), []byte(`{"listen":`), 0600); err != nil {
		t.Fatal(err)
	}
	path, err := prepareNode(seed, dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = canonicalSeed(data); err != nil {
		t.Fatal("interrupted temporary file became active", err)
	}
	if _, err = prepareNode(seed, dir); err != nil {
		t.Fatal("completed initialization was not reusable", err)
	}
}
