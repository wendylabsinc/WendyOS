package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/internal/worldmcp"
	"golang.org/x/sys/unix"
)

func privateConfig(t *testing.T) []byte {
	t.Helper()
	_, root, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	grant, err := authority.SignGrant(authority.Grant{Version: 1, ID: "agent", Tenant: "t", Site: "s", Domain: "simulation", Principal: "agent", Kind: "agent", Source: world.Source{Device: "d", App: "a", Service: "s", Stream: "x"}, PublicKey: pub, Scopes: []string{"read"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(client.Config{URL: "http://127.0.0.1:1", Audience: "node", Grant: grant, PrivateKey: key, AllowInsecureSimulation: true})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestPrivateConfigurationFileBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	raw := privateConfig(t)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worldmcp.New(cfg); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cfg.PrivateKey, mustConfig(t, raw).PrivateKey) {
		t.Fatal("changed credentials")
	}
	for name, prepare := range map[string]func(string){
		"missing": func(string) {},
		"public": func(p string) {
			if err := os.WriteFile(p, raw, 0644); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(p string) {
			if err := os.Mkdir(p, 0700); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(p string) {
			if err := os.Symlink(path, p); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(p string) {
			if err := unix.Mkfifo(p, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"too-large": func(p string) {
			if err := os.WriteFile(p, bytes.Repeat([]byte(" "), authority.MaxEnvelopeBytes+1), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"invalid-json": func(p string) {
			if err := os.WriteFile(p, []byte(`{"secret":"do-not-print"}`), 0600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, name)
			prepare(p)
			_, err := loadConfig(p)
			if !errors.Is(err, errStartup) || strings.Contains(err.Error(), p) || strings.Contains(err.Error(), "do-not-print") {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
}
func mustConfig(t *testing.T, b []byte) client.Config {
	t.Helper()
	var cfg client.Config
	if json.Unmarshal(b, &cfg) != nil {
		t.Fatal("fixture")
	}
	return cfg
}
func TestInvalidCLIArgumentsNeverServeOrEchoInput(t *testing.T) {
	for _, args := range [][]string{{}, {"--url", "https://private.invalid"}, {"--credentials", "/private/do-not-print", "--sign"}, {"extra"}} {
		if err := run(context.Background(), args); !errors.Is(err, errStartup) {
			t.Fatalf("unexpected error %v", err)
		}
	}
}
