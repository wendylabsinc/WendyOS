package agentvmfixture

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/agentmodule"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
)

func commissioningFixture(t *testing.T) (dir, state, specPath string, spec CommissionSpec) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, state = filepath.Join(base, "fixture"), filepath.Join(base, "state")
	if _, err := InitializeEnrollment(dir, state); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, "scene-read.token")
	if err := writeNew(tokenPath, []byte(strings.Repeat("t", 64))); err != nil {
		t.Fatal(err)
	}
	specPath = filepath.Join(dir, "commission.json")
	spec = CommissionSpec{IsolatedSimulationNetwork: true, SceneURL: "http://10.34.7.2:8091", ReadTokenFile: tokenPath, RunID: "mm-test-1", BootID: "observed-scene-boot", PackageSHA256: strings.Repeat("a", 64), Frame: world.FrameRef{ID: "office-floor", Revision: "observed-revision"}, AnchorUnixNS: 1780000000000000000, Site: "office", AppID: MultimodalApp}
	return
}

func TestMultimodalCommissionPinsObservedSceneAndExactServices(t *testing.T) {
	dir, state, path, s := commissioningFixture(t)
	if err := writeJSON(path, s); err != nil {
		t.Fatal(err)
	}
	r, err := CommissionMultimodal(dir, state, path)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != Kind || r.State != "configured_not_activated" || r.ClockDomain != "simulation:"+s.RunID || r.BootID != s.BootID || r.AppID != MultimodalApp {
		t.Fatal("misleading commissioning receipt", r)
	}
	raw, err := agentmodule.ReadCommissionedFile(r.ConfigPath, 64<<10)
	var cfg agentmodule.FileConfig
	if err != nil || jws.DecodeStrict(raw, &cfg) != nil || digest(raw) != r.ConfigSHA256 {
		t.Fatal("configuration bytes disagree with receipt", err)
	}
	p, err := readPublic(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policy.Tenant != p.Tenant || cfg.Policy.Device != p.Principal || cfg.Policy.ExecutionDomain != "simulation" || cfg.Policy.SampleClockDomain != r.ClockDomain || len(cfg.RootsDER) != 1 || !bytes.Equal(cfg.RootsDER[0], p.RootDER) || cfg.SimulationClockFile != "" || cfg.SimulationClock == nil {
		t.Fatal("generated identity or source clock changed")
	}
	c := cfg.SimulationClock
	if c.URL != s.SceneURL || c.ReadTokenFile != s.ReadTokenFile || c.RunID != s.RunID || c.BootID != s.BootID || c.PackageSHA256 != s.PackageSHA256 || c.Frame != s.Frame || c.AnchorUnixNS != s.AnchorUnixNS {
		t.Fatal("scene pin changed")
	}
	want := map[string]string{"camera-a": "camera-a", "gas": "gas-1", "audio": "audio-1", "sensorlink-bridge": "camera-b"}
	if len(cfg.Policy.Grants) != 5 {
		t.Fatal("broadened grant set")
	}
	for _, g := range cfg.Policy.Grants {
		if g.App != MultimodalApp || len(g.Publish) != 1 {
			t.Fatal("wrong app or publication set")
		}
		if g.Service == "fusion" {
			if g.Publish[0].ComponentType != resultrecord.ComponentType || g.Publish[0].Stream != "hazard" || len(g.Subscribe) != 4 {
				t.Fatal("wrong fusion grant")
			}
			for _, sub := range g.Subscribe {
				if sub.App != MultimodalApp || sub.ComponentType != sensorrecord.ComponentType || want[sub.Service] != sub.Stream {
					t.Fatal("broadened source read", sub)
				}
			}
		} else if want[g.Service] != g.Publish[0].Stream || g.Publish[0].ComponentType != sensorrecord.ComponentType || len(g.Subscribe) != 0 {
			t.Fatal("broadened sensor grant", g)
		}
	}
	if cfg.Limits.MessagesPerSecond != 200 || cfg.Limits.MessageBurst != 400 || cfg.Limits.BytesPerSecond != 8<<20 || cfg.Limits.ByteBurst != 16<<20 {
		t.Fatal("missing explicit workload budget")
	}
	if cfg.ProofArchivePath != filepath.Join(state, "archive", "proofs.db") || cfg.EvidenceDirectory != filepath.Join(state, "evidence") || cfg.JournalPath != filepath.Join(state, "journal", "journal.db") {
		t.Fatal("store escaped commissioned state root")
	}
	entries, err := os.ReadDir(state)
	if err != nil || len(entries) != 0 {
		t.Fatal("writing policy activated World stores", err)
	}
	if !missing(p.SimulationClockPath) {
		t.Fatal("created mutable fixture clock")
	}
	receipt, _ := json.Marshal(r)
	if r.HistoryReader || bytes.Contains(receipt, []byte("history_reader")) {
		t.Fatal("default receipt opted into historical access")
	}
	if bytes.Contains(raw, bytes.Repeat([]byte("t"), 64)) || bytes.Contains(receipt, bytes.Repeat([]byte("t"), 64)) || bytes.Contains(receipt, []byte("issuer_key")) {
		t.Fatal("commissioning leaked credentials")
	}
	if _, err := CommissionMultimodal(dir, state, path); err == nil {
		t.Fatal("replaced commissioned policy")
	}
	after, _ := os.ReadFile(r.ConfigPath)
	if !bytes.Equal(raw, after) {
		t.Fatal("failed recommission changed policy")
	}
}

func TestMultimodalHistoryReaderIsExplicitExactAndReadOnly(t *testing.T) {
	dir, state, path, s := commissioningFixture(t)
	s.HistoryReader = true
	if err := writeJSON(path, s); err != nil {
		t.Fatal(err)
	}
	r, err := CommissionMultimodal(dir, state, path)
	if err != nil || !r.HistoryReader || r.State != "configured_not_activated" {
		t.Fatal("history commissioning failed or claimed activation", err)
	}
	raw, err := agentmodule.ReadCommissionedFile(r.ConfigPath, 64<<10)
	var cfg agentmodule.FileConfig
	if err != nil || jws.DecodeStrict(raw, &cfg) != nil || digest(raw) != r.ConfigSHA256 {
		t.Fatal("history config differs from receipt", err)
	}
	if len(cfg.Policy.Grants) != 6 {
		t.Fatal("history changed unrelated grants")
	}
	reader := cfg.Policy.Grants[5]
	if reader.App != MultimodalApp || reader.Service != MultimodalProofReader || len(reader.Publish) != 0 || len(reader.Subscribe) != 5 {
		t.Fatal("history reader acquired extra authority")
	}
	want := map[serviceaccess.Source]bool{
		{App: MultimodalApp, Service: "camera-a", ComponentType: sensorrecord.ComponentType, Stream: "camera-a"}:          true,
		{App: MultimodalApp, Service: "gas", ComponentType: sensorrecord.ComponentType, Stream: "gas-1"}:                  true,
		{App: MultimodalApp, Service: "audio", ComponentType: sensorrecord.ComponentType, Stream: "audio-1"}:              true,
		{App: MultimodalApp, Service: "sensorlink-bridge", ComponentType: sensorrecord.ComponentType, Stream: "camera-b"}: true,
		{App: MultimodalApp, Service: "fusion", ComponentType: resultrecord.ComponentType, Stream: "hazard"}:              true,
	}
	for _, source := range reader.Subscribe {
		if !want[source] {
			t.Fatal("duplicate or broadened historical source", source)
		}
		delete(want, source)
	}
	if len(want) != 0 || len(cfg.Policy.Grants[4].Subscribe) != 4 || len(cfg.Policy.Grants[4].Publish) != 1 {
		t.Fatal("history changed fusion inputs or omitted source")
	}
	for _, grant := range cfg.Policy.Grants[:5] {
		if grant.Service == MultimodalProofReader || len(grant.Publish) != 1 {
			t.Fatal("default publishers changed")
		}
	}
	entries, err := os.ReadDir(state)
	if err != nil || len(entries) != 0 {
		t.Fatal("optional grant initialized stores or replayed proofs", err)
	}
	public, err := readPublic(dir)
	if err != nil || !missing(public.SimulationClockPath) {
		t.Fatal("history created a mutable source clock", err)
	}
	receipt, _ := json.Marshal(r)
	for _, data := range [][]byte{raw, receipt} {
		for _, field := range []string{"generation", "private_key", "issuer_key", "retain_for_ms", "retention_seconds", strings.Repeat("t", 64)} {
			if bytes.Contains(data, []byte(field)) {
				t.Fatal("commissioner supplied runtime identity, secret or retention", field)
			}
		}
	}
	before := bytes.Clone(raw)
	if _, err := CommissionMultimodal(dir, state, path); err == nil {
		t.Fatal("history flag replaced existing authority")
	}
	after, err := agentmodule.ReadCommissionedFile(r.ConfigPath, 64<<10)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("recommission changed archive policy", err)
	}
}

func TestMultimodalHistoryFlagRequiresBooleanAndCannotSupplyAuthority(t *testing.T) {
	for _, value := range []string{"null", `"true"`, "1", "[]", "{}"} {
		t.Run(value, func(t *testing.T) {
			dir, state, path, spec := commissioningFixture(t)
			raw, _ := json.Marshal(spec)
			raw = append(raw[:len(raw)-1], []byte(`,"history_reader":`+value+`}`)...)
			if err := writeNew(path, raw); err != nil {
				t.Fatal(err)
			}
			if _, err := CommissionMultimodal(dir, state, path); err == nil {
				t.Fatal("nonboolean history flag accepted")
			}
			if !missing(filepath.Join(dir, "world.json")) {
				t.Fatal("rejected history flag created policy")
			}
		})
	}
	for _, extra := range []string{`"history_reader":true,"history_reader":false`, `"history_reader":true,"history_generation":7`, `"history_reader":true,"history_service":"other"`, `"history_reader":true,"retain_for_ms":600000`} {
		t.Run(extra, func(t *testing.T) {
			dir, state, path, spec := commissioningFixture(t)
			raw, _ := json.Marshal(spec)
			raw = append(raw[:len(raw)-1], []byte(","+extra+"}")...)
			if err := writeNew(path, raw); err != nil {
				t.Fatal(err)
			}
			if _, err := CommissionMultimodal(dir, state, path); err == nil {
				t.Fatal("history fields overrode fixed authority")
			}
		})
	}
	t.Run("explicit-false", func(t *testing.T) {
		dir, state, path, spec := commissioningFixture(t)
		raw, _ := json.Marshal(spec)
		raw = append(raw[:len(raw)-1], []byte(`,"history_reader":false}`)...)
		if err := writeNew(path, raw); err != nil {
			t.Fatal(err)
		}
		receipt, err := CommissionMultimodal(dir, state, path)
		if err != nil || receipt.HistoryReader {
			t.Fatal("false enabled history", err)
		}
		var cfg agentmodule.FileConfig
		data, err := agentmodule.ReadCommissionedFile(receipt.ConfigPath, 64<<10)
		if err != nil || jws.DecodeStrict(data, &cfg) != nil || len(cfg.Policy.Grants) != 5 {
			t.Fatal("false changed default scope", err)
		}
	})
}

func TestMultimodalCommissionRejectsInvalidOrUnprotectedInputs(t *testing.T) {
	for name, change := range map[string]func(*testing.T, string, string, *CommissionSpec){
		"not-isolated":   func(_ *testing.T, _, _ string, s *CommissionSpec) { s.IsolatedSimulationNetwork = false },
		"foreign-app":    func(_ *testing.T, _, _ string, s *CommissionSpec) { s.AppID = "com.other.app" },
		"public-address": func(_ *testing.T, _, _ string, s *CommissionSpec) { s.SceneURL = "http://8.8.8.8:8091" },
		"hostname":       func(_ *testing.T, _, _ string, s *CommissionSpec) { s.SceneURL = "http://scene:8091" },
		"url-secret":     func(_ *testing.T, _, _ string, s *CommissionSpec) { s.SceneURL = "http://secret@10.34.7.2:8091" },
		"url-query":      func(_ *testing.T, _, _ string, s *CommissionSpec) { s.SceneURL += "?token=forbidden" },
		"missing-boot":   func(_ *testing.T, _, _ string, s *CommissionSpec) { s.BootID = "" },
		"bad-run":        func(_ *testing.T, _, _ string, s *CommissionSpec) { s.RunID = "../another" },
		"bad-digest":     func(_ *testing.T, _, _ string, s *CommissionSpec) { s.PackageSHA256 = strings.Repeat("A", 64) },
		"missing-anchor": func(_ *testing.T, _, _ string, s *CommissionSpec) { s.AnchorUnixNS = 0 },
		"public-token": func(t *testing.T, _, _ string, s *CommissionSpec) {
			if err := os.Chmod(s.ReadTokenFile, 0644); err != nil {
				t.Fatal(err)
			}
		},
		"outside-token": func(t *testing.T, dir, _ string, s *CommissionSpec) {
			p := filepath.Join(filepath.Dir(dir), "outside.token")
			if err := writeNew(p, []byte(strings.Repeat("t", 64))); err != nil {
				t.Fatal(err)
			}
			s.ReadTokenFile = p
		},
		"linked-token": func(t *testing.T, dir, _ string, s *CommissionSpec) {
			p := filepath.Join(dir, "linked.token")
			if err := os.Symlink(s.ReadTokenFile, p); err != nil {
				t.Fatal(err)
			}
			s.ReadTokenFile = p
		},
		"existing-state": func(t *testing.T, _, state string, _ *CommissionSpec) {
			if err := writeNew(filepath.Join(state, "previous-state"), []byte("keep")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, state, path, s := commissioningFixture(t)
			change(t, dir, state, &s)
			if err := writeJSON(path, s); err != nil {
				t.Fatal(err)
			}
			if _, err := CommissionMultimodal(dir, state, path); err == nil {
				t.Fatal("invalid commissioning succeeded")
			}
			if !missing(filepath.Join(dir, "world.json")) {
				t.Fatal("refusal created policy")
			}
		})
	}
	t.Run("unknown-field", func(t *testing.T) {
		dir, state, path, s := commissioningFixture(t)
		raw, _ := json.Marshal(s)
		raw = append(raw[:len(raw)-1], []byte(`,"publish":["*"]}`)...)
		if err := writeNew(path, raw); err != nil {
			t.Fatal(err)
		}
		if _, err := CommissionMultimodal(dir, state, path); err == nil {
			t.Fatal("unknown grant extension accepted")
		}
		if !missing(filepath.Join(dir, "world.json")) {
			t.Fatal("refusal created policy")
		}
	})
}
