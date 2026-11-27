//go:build !world_wendy_boot

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
)

func options(t *testing.T) stageOptions {
	t.Helper()
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return stageOptions{Out: filepath.Join(t.TempDir(), "lab"), Repo: repo, RunID: "vm-test", Arch: "arm64", Port: 18881, Transport: "host-http", AllowInsecureSimulation: true, Now: func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }}
}
func fakeBuild(_ string, path, arch, tag, target string) error {
	return writeNew(path, []byte("test-binary "+arch+" "+tag+" "+target), 0755)
}
func decodeFile(t *testing.T, path string, value any) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, value); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStageSecretsOutsideAllowlistedImageAndNarrowAuthority(t *testing.T) {
	opts := options(t)
	var builds []string
	m, err := stage(opts, func(repo, path, arch, tag, target string) error {
		builds = append(builds, arch+":"+tag+":"+target)
		return fakeBuild(repo, path, arch, tag, target)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(builds, []string{"arm64::./cmd/worldd", "arm64:world_wendy_boot:./cmd/world-wendy-lab"}) {
		t.Fatal("unexpected executable inventory", builds)
	}
	project := filepath.Join(opts.Out, "project")
	entries, err := os.ReadDir(project)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !reflect.DeepEqual(names, []string{".dockerignore", "Dockerfile", "node-seed.json", "wendy.json", "world-wendy-boot", "worldd"}) {
		t.Fatal("unexpected Docker context", names)
	}
	var seed seedConfig
	seedBytes := decodeFile(t, filepath.Join(project, "node-seed.json"), &seed)
	if _, err = canonicalSeed(seedBytes); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private_key", "tasks", "task_relay", "evidence", "peers", "peer_grant"} {
		if bytes.Contains(seedBytes, []byte(`"`+forbidden+`"`)) {
			t.Fatal("image carries forbidden configuration", forbidden)
		}
	}
	var app struct {
		AppID        string           `json:"appId"`
		Platform     string           `json:"platform"`
		Entitlements []map[string]any `json:"entitlements"`
		Readiness    struct {
			TCP struct {
				Port int `json:"port"`
			} `json:"tcpSocket"`
		} `json:"readiness"`
	}
	decodeFile(t, filepath.Join(project, "wendy.json"), &app)
	if app.AppID != "com.wendy.world.vm.vm-test" || app.Platform != "linux/arm64" || app.Readiness.TCP.Port != 18881 || len(app.Entitlements) != 3 {
		t.Fatal("unexpected app contract", app)
	}
	if app.Entitlements[0]["type"] != "network" || app.Entitlements[0]["mode"] != "host" || app.Entitlements[1]["type"] != "http" || app.Entitlements[1]["port"] != float64(18881) || app.Entitlements[2]["type"] != "persist" || app.Entitlements[2]["path"] != "/data" || app.Entitlements[2]["name"] != m.VolumeName {
		t.Fatal("unexpected entitlements", app.Entitlements)
	}
	if m.ExpiresAtUnixMS-m.IssuedAtUnixMS != (59*time.Minute).Milliseconds() || m.Transport != "host-http" {
		t.Fatal("unbounded or misleading test scope", m)
	}
	for path, want := range m.Artifacts {
		b, e := os.ReadFile(filepath.Join(opts.Out, path))
		if e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != want {
			t.Fatal("manifest hash mismatch", path)
		}
	}
	var trust struct {
		Root ed25519.PublicKey           `json:"root_public_key"`
		Rev  authority.SignedRevocations `json:"revocations"`
	}
	decodeFile(t, filepath.Join(opts.Out, "clients", "trust.json"), &trust)
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: trust.Root, Tenant: m.Tenant, Site: m.Site, Domain: "simulation", Audience: m.Audience, Now: opts.Now, Replay: authority.NewMemoryReplay(16)})
	if err != nil {
		t.Fatal(err)
	}
	if err = v.UpdateRevocations(trust.Rev); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"publisher", "reader"} {
		path := filepath.Join(opts.Out, "clients", name+".json")
		var cfg client.Config
		decodeFile(t, path, &cfg)
		info, e := os.Stat(path)
		if e != nil || info.Mode().Perm() != 0600 {
			t.Fatal("credential permissions", name, e)
		}
		if _, e = client.New(cfg); e != nil {
			t.Fatal("invalid generated credentials", e)
		}
		if _, e = v.VerifyGrant(cfg.Grant, "read"); e != nil {
			t.Fatal(e)
		}
		_, publishErr := v.VerifyGrant(cfg.Grant, "publish")
		if (publishErr == nil) != (name == "publisher") {
			t.Fatal("wrong publication authority", name, publishErr)
		}
		for _, op := range []string{"mission", "stop", "execute", "site-publish", "annotation-write", "evidence-write", "replicate"} {
			if _, e = v.VerifyGrant(cfg.Grant, op); e == nil {
				t.Fatal("unexpected delegated scope", name, op)
			}
		}
		for _, entry := range entries {
			b, e := os.ReadFile(filepath.Join(project, entry.Name()))
			if e != nil {
				t.Fatal(e)
			}
			key, _ := json.Marshal(cfg.PrivateKey)
			if bytes.Contains(b, key[1:len(key)-1]) {
				t.Fatal("client signing key entered image context", name)
			}
		}
	}
	for _, dir := range []string{opts.Out, project, filepath.Join(opts.Out, "clients")} {
		info, e := os.Stat(dir)
		if e != nil || info.Mode().Perm() != 0700 {
			t.Fatal("private staging directory mode", dir, e)
		}
	}
}

func TestStageRefusesReplacementInvalidScopeAndCleansFailedBuild(t *testing.T) {
	opts := options(t)
	if _, err := stage(opts, fakeBuild); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(opts.Out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stage(opts, fakeBuild); err == nil {
		t.Fatal("existing output overwritten")
	}
	after, _ := os.ReadFile(filepath.Join(opts.Out, "manifest.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("existing stage changed")
	}
	for name, change := range map[string]func(*stageOptions){"no acknowledgement": func(o *stageOptions) { o.AllowInsecureSimulation = false }, "implicit transport": func(o *stageOptions) { o.Transport = "" }, "physical": func(o *stageOptions) { o.Transport = "physical" }, "traversal": func(o *stageOptions) { o.RunID = "../x" }, "long run": func(o *stageOptions) { o.RunID = strings.Repeat("a", 33) }, "privileged port": func(o *stageOptions) { o.Port = 80 }, "bad arch": func(o *stageOptions) { o.Arch = "darwin" }} {
		t.Run(name, func(t *testing.T) {
			o := options(t)
			change(&o)
			if _, e := stage(o, fakeBuild); e == nil {
				t.Fatal("invalid stage accepted")
			}
			if _, e := os.Stat(o.Out); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("invalid stage left output", e)
			}
		})
	}
	failed := options(t)
	if _, err = stage(failed, func(string, string, string, string, string) error { return errors.New("injected compiler failure") }); err == nil {
		t.Fatal("failed build succeeded")
	}
	if _, err = os.Stat(failed.Out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed build left partial credentials", err)
	}
}
