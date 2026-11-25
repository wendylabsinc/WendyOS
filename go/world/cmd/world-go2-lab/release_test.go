package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
)

func commissionedFixture(t *testing.T) (options, releaseOutput, commissionedOutput, string) {
	t.Helper()
	o := testOptions(t)
	dir := filepath.Join(t.TempDir(), "release")
	r, e := release(releaseOptions{Out: dir, Repo: o.Repo, Arch: o.Arch}, fakeBuilder)
	if e != nil {
		t.Fatal(e)
	}
	c, e := commission(o, dir, "localhost:5000/private/go2@sha256:"+strings.Repeat("d", 64))
	if e != nil {
		t.Fatal(e)
	}
	return o, r, c, dir
}
func TestReusableReleaseTwoRunsNeverChangeImageInputs(t *testing.T) {
	a, r, c, dir := commissionedFixture(t)
	before := map[string][]byte{}
	for name := range r.Files {
		before[name], _ = os.ReadFile(filepath.Join(dir, name))
	}
	b := testOptions(t)
	var binding map[string]any
	raw, _ := os.ReadFile(b.Binding)
	_ = json.Unmarshal(raw, &binding)
	binding["run_id"] = "other-scene"
	binding["world_epoch"] = 3
	binding["project_revision"] = strings.Repeat("c", 64)
	if e := os.WriteFile(b.Binding, jsonBytes(binding), 0600); e != nil {
		t.Fatal(e)
	}
	second, e := commission(b, dir, c.Image)
	if e != nil {
		t.Fatal(e)
	}
	if c.Bundle.AppID != second.Bundle.AppID || c.Bundle.Service != second.Bundle.Service || c.Bundle.Assignee != second.Bundle.Assignee || c.Bundle.ReleaseID != second.Bundle.ReleaseID || c.Image != second.Image || c.Bundle.Volume == second.Bundle.Volume || c.BundleSHA256 == second.BundleSHA256 {
		t.Fatal("run/image identities")
	}
	for name, raw := range before {
		after, _ := os.ReadFile(filepath.Join(dir, name))
		if !bytes.Equal(raw, after) {
			t.Fatal("run changed image inputs", name)
		}
	}
	for _, o := range []options{a, b} {
		var cfg client.Config
		readJSON(t, filepath.Join(o.Out, "clients/operator.json"), &cfg)
		secret := []byte(base64.StdEncoding.EncodeToString(cfg.PrivateKey))
		principal := []byte(cfg.Grant.Grant.Principal)
		for _, context := range []string{dir, filepath.Join(o.Out, "carrier"), filepath.Join(o.Out, "mission")} {
			if e := filepath.WalkDir(context, func(path string, d os.DirEntry, e error) error {
				if e != nil {
					return e
				}
				if d.IsDir() {
					return nil
				}
				raw, e := os.ReadFile(path)
				if e != nil {
					return e
				}
				if bytes.Contains(raw, secret) || bytes.Contains(raw, principal) {
					t.Fatal("operator authority entered deploy context", path)
				}
				return nil
			}); e != nil {
				t.Fatal(e)
			}
		}
		var compose map[string]any
		readJSON(t, filepath.Join(o.Out, "mission/compose.yaml"), &compose)
		svc := compose["services"].(map[string]any)[motionService].(map[string]any)
		if _, ok := svc["build"]; ok || svc["image"] != c.Image {
			t.Fatal("motion image is rebuilt")
		}
		var installer map[string]any
		readJSON(t, filepath.Join(o.Out, "carrier/wendy.json"), &installer)
		ents := installer["entitlements"].([]any)
		if len(ents) != 2 || ents[0].(map[string]any)["mode"] != "none" || ents[1].(map[string]any)["type"] != "persist" {
			t.Fatal("installer capabilities")
		}
	}
	fresh, e := release(releaseOptions{Out: filepath.Join(t.TempDir(), "release"), Repo: a.Repo, Arch: a.Arch}, fakeBuilder)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(r, fresh) {
		t.Fatal("release contains run or timestamp entropy")
	}
}

func TestRunValidationScopeExpiryAndTampering(t *testing.T) {
	o, _, c, _ := commissionedFixture(t)
	dir := filepath.Join(o.Out, "carrier/bundle")
	check := func(current bool, now time.Time) error {
		return validateRun(dir, c.BundleSHA256, c.Bundle.ReleaseID, c.Bundle.ImageDigest, current, func() time.Time { return now })
	}
	if e := check(true, o.Now()); e != nil {
		t.Fatal(e)
	}
	if e := check(true, o.Now().Add(59*time.Minute)); e == nil {
		t.Fatal("expired run authorizes launch")
	}
	if e := check(true, o.Now().Add(-time.Millisecond)); e == nil {
		t.Fatal("clock before commissioning accepted")
	}
	if e := check(false, o.Now().Add(time.Hour)); e != nil {
		t.Fatal("historical integrity unexpectedly renews or depends on now", e)
	}
	if e := validateRun(dir, c.BundleSHA256, strings.Repeat("f", 64), c.Bundle.ImageDigest, false, o.Now); e == nil {
		t.Fatal("different release accepted")
	}
	// A self-consistent replacement manifest is still constrained by the fixed
	// validator's catalog, paths, cryptographic grants and exact source identities.
	cases := map[string]func(map[string]any){
		"broader catalog":  func(n map[string]any) { n["tasks"].(map[string]any)["catalog"] = map[string]any{"anything": "MOVE"} },
		"physical clock":   func(n map[string]any) { n["clock_domain"] = "physical" },
		"outside state":    func(n map[string]any) { n["data_path"] = "/tmp/unfenced" },
		"new peers":        func(n map[string]any) { n["peers"] = []any{map[string]any{"url": "http://remote"}} },
		"unknown behavior": func(n map[string]any) { n["simulation_faults"] = map[string]any{"task_commit_delay_ms": 999} },
		"changed signature": func(n map[string]any) {
			n["peer_grant"].(map[string]any)["signature"] = base64.StdEncoding.EncodeToString(make([]byte, 64))
		},
	}
	nodePath := filepath.Join(dir, "node.json")
	original, _ := os.ReadFile(nodePath)
	manifestPath := filepath.Join(dir, "run.json")
	originalManifest, _ := os.ReadFile(manifestPath)
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			var n map[string]any
			_ = json.Unmarshal(original, &n)
			change(n)
			changed := jsonBytes(n)
			m := c.Bundle
			m.Files = map[string]fileDigest{}
			for k, v := range c.Bundle.Files {
				m.Files[k] = v
			}
			m.Files["node.json"] = digest(changed)
			manifest := jsonBytes(m)
			_ = os.WriteFile(nodePath, changed, 0600)
			_ = os.WriteFile(manifestPath, manifest, 0600)
			if e := validateRun(dir, digest(manifest).SHA256, m.ReleaseID, m.ImageDigest, false, o.Now); e == nil {
				t.Fatal("invalid replacement accepted")
			}
		})
	}
	_ = os.WriteFile(nodePath, original, 0600)
	_ = os.WriteFile(manifestPath, originalManifest, 0600)
	if e := check(true, o.Now()); e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(filepath.Join(dir, "unexpected"), []byte("extra"), 0600)
	if e := check(false, o.Now()); e == nil {
		t.Fatal("extra executable/data file accepted")
	}
}

func TestReleaseAndRunInputsRefuseMutableReferencesAndFiles(t *testing.T) {
	o, _, c, dir := commissionedFixture(t)
	for _, ref := range []string{"image:latest", "private/go2@sha256:" + strings.Repeat("a", 64), "https://registry.test/go2@sha256:" + strings.Repeat("a", 64), "localhost:5000/a/../b@sha256:" + strings.Repeat("a", 64)} {
		if _, e := imageDigest(ref); e == nil {
			t.Fatal("accepted image", ref)
		}
	}
	nodePath := filepath.Join(o.Out, "carrier/bundle/node.json")
	other := nodePath + ".original"
	if e := os.Rename(nodePath, other); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(other, nodePath); e != nil {
		t.Fatal(e)
	}
	if e := validateRun(filepath.Dir(nodePath), c.BundleSHA256, c.Bundle.ReleaseID, c.Bundle.ImageDigest, false, o.Now); e == nil {
		t.Fatal("symlink run accepted")
	}
	source := filepath.Join(dir, "project/boot.py")
	if e := os.WriteFile(source, []byte("changed"), 0600); e != nil {
		t.Fatal(e)
	}
	o.Out = filepath.Join(t.TempDir(), "new-run")
	if _, e := commission(o, dir, c.Image); e == nil {
		t.Fatal("changed release accepted")
	}
	if _, e := os.Stat(o.Out); !os.IsNotExist(e) {
		t.Fatal("issued credentials before release validation")
	}
}
