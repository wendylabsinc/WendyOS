package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
)

func TestMissionLabUsesIndependentBoundedSupervisors(t *testing.T) {
	dir := t.TempDir()
	if err := generateOptions(dir, "/state", "supervised", 19001, true, true, false); err != nil {
		t.Fatal(err)
	}
	var trust struct {
		RootPublicKey []byte                      `json:"root_public_key"`
		Revocations   authority.SignedRevocations `json:"revocations"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "site-trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &trust); err != nil {
		t.Fatal(err)
	}
	principals, keys := map[string]bool{}, map[string]bool{}
	for _, name := range []string{"supervisor-a.json", "supervisor-b.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var cfg client.Config
		if err = json.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		g := cfg.Grant.Grant
		if g.Kind != "operator" || g.Domain != "simulation" || cfg.Audience != "node-1" || !reflect.DeepEqual(g.Scopes, []string{"supervise", "stop", "read"}) {
			t.Fatal("unexpected supervisor authority")
		}
		key := authority.KeyID(g.PublicKey)
		if principals[g.Principal] || keys[key] {
			t.Fatal("supervisors share identity or key")
		}
		principals[g.Principal], keys[key] = true, true
		v, err := authority.NewVerifier(authority.Config{RootPublicKey: trust.RootPublicKey, Tenant: g.Tenant, Site: g.Site, Domain: g.Domain, Audience: cfg.Audience, Now: time.Now, Replay: authority.NewMemoryReplay(64)})
		if err != nil {
			t.Fatal(err)
		}
		if err = v.UpdateRevocations(trust.Revocations); err != nil {
			t.Fatal(err)
		}
		if _, err = v.VerifyGrant(cfg.Grant, "supervise"); err != nil {
			t.Fatal(err)
		}
		if _, err = v.VerifyGrant(cfg.Grant, "mission"); err == nil {
			t.Fatal("supervisor can issue missions")
		}
		if _, err = client.New(cfg); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("credential permissions", err)
		}
	}
	var node struct {
		PeerGrant authority.SignedGrant `json:"peer_grant"`
		Tasks     struct {
			Enable bool `json:"enable_supervision"`
		} `json:"tasks"`
	}
	data, err = os.ReadFile(filepath.Join(dir, "node-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &node); err != nil {
		t.Fatal(err)
	}
	if !node.Tasks.Enable || node.PeerGrant.Grant.TaskStatusAudience != "node-1" || node.PeerGrant.Grant.TaskStatusAssignee != "sim-robot/navigation" {
		t.Fatal("local gateway supervision not bound")
	}
}

func TestLoadPublisherIsSimulationOnlyAndOwnedByNodeTwo(t *testing.T) {
	for _, containers := range []bool{false, true} {
		dir := t.TempDir()
		if err := generateOptions(dir, "/state", "load-test", 19001, containers, true, false); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "load-publisher.json"))
		if err != nil {
			t.Fatal(err)
		}
		var cfg client.Config
		if err = json.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		g := cfg.Grant.Grant
		if g.Domain != "simulation" || g.Kind != "app" || g.PublishAudience != "node-2" || cfg.Audience != "node-2" || !reflect.DeepEqual(g.Scopes, []string{"publish", "read"}) || g.Source.Device != "sim-load-publisher" {
			t.Fatal("load publisher authority is not confined to generated simulation observations")
		}
		wantURL := "http://127.0.0.1:19002"
		if containers {
			wantURL = "http://world-node-2:8080"
		}
		if cfg.URL != wantURL || !cfg.AllowInsecureSimulation {
			t.Fatal("load publisher ingress mismatch")
		}
		var node struct {
			RootPublicKey []byte                      `json:"root_public_key"`
			Revocations   authority.SignedRevocations `json:"revocations"`
		}
		data, err = os.ReadFile(filepath.Join(dir, "node-2.json"))
		if err != nil || json.Unmarshal(data, &node) != nil {
			t.Fatal("node trust unavailable")
		}
		v, err := authority.NewVerifier(authority.Config{RootPublicKey: node.RootPublicKey, Tenant: g.Tenant, Site: g.Site, Domain: "simulation", Audience: "node-2", Now: time.Now, Replay: authority.NewMemoryReplay(8)})
		if err != nil || v.UpdateRevocations(node.Revocations) != nil {
			t.Fatal("node trust invalid")
		}
		if _, err = v.VerifyGrant(cfg.Grant, "publish"); err != nil {
			t.Fatal(err)
		}
		if _, err = client.New(cfg); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(dir, "load-publisher.json"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("load profile is not private")
		}
	}
}

func TestNativeContributorsHaveSeparateAnnotationOnlyIdentities(t *testing.T) {
	dir := t.TempDir()
	if err := generateOptions(dir, "", "annotations", 19001, false, false, true); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	var owner struct {
		Root  []byte `json:"root_public_key"`
		Clock struct {
			Grant authority.SignedGrant `json:"grant"`
			Key   []byte                `json:"private_key"`
		} `json:"annotation_clock"`
	}
	ownerRaw, err := os.ReadFile(filepath.Join(dir, "node-3.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ownerRaw, &owner); err != nil {
		t.Fatal(err)
	}
	clockGrant := owner.Clock.Grant.Grant
	if clockGrant.Kind != "device" || clockGrant.Domain != "simulation" || clockGrant.PublishAudience != "node-3" || !reflect.DeepEqual(clockGrant.Scopes, []string{"annotation-clock-server"}) || len(owner.Clock.Key) != 64 || !bytes.Equal(owner.Clock.Key[32:], clockGrant.PublicKey) {
		t.Fatal("clock identity is not a distinct narrow simulation credential")
	}
	for _, platform := range []string{"apple", "macos", "ios", "visionos", "android"} {
		data, err := os.ReadFile(filepath.Join(dir, "contributor-"+platform+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var cfg client.Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		g := cfg.Grant.Grant
		if g.Domain != "simulation" || g.Kind != "operator" || g.PublishAudience != "node-3" || cfg.Audience != "node-3" ||
			g.Source.Device != "sim-contributor-"+platform || !reflect.DeepEqual(g.Scopes, []string{"read", "evidence-read", "annotation-write"}) {
			t.Fatal("unexpected contribution authority", g)
		}
		key := authority.KeyID(g.PublicKey)
		pins := cfg.ClockTrust
		if pins == nil || !bytes.Equal(pins.RootPublicKey, owner.Root) || pins.GatewayPrincipal != clockGrant.Principal || pins.GatewayKeySHA256 != authority.KeyID(clockGrant.PublicKey) || pins.RunID != "annotations" || pins.ClockDomain != "simulation:annotations" || pins.GatewayKeySHA256 == key {
			t.Fatal("contributor clock pins do not match the commissioned gateway")
		}
		if keys[key] {
			t.Fatal("platforms share an author key")
		}
		keys[key] = true
	}
}

func TestLabGeneratesOnlyPrivateSimulationCredentials(t *testing.T) {
	dir := t.TempDir()
	if e := generate(dir, "", "test", 19001, false); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"node-1.json", "node-2.json", "node-3.json", "client-1.json", "client-2.json", "client-3.json", "issuer.json"} {
		info, e := os.Stat(filepath.Join(dir, name))
		if e != nil {
			t.Fatal(e)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatal(name, info.Mode())
		}
	}
	b, e := os.ReadFile(filepath.Join(dir, "client-1.json"))
	if e != nil {
		t.Fatal(e)
	}
	var cfg client.Config
	if e = json.Unmarshal(b, &cfg); e != nil {
		t.Fatal(e)
	}
	if cfg.Grant.Grant.Domain != "simulation" {
		t.Fatal("non-simulation lab grant")
	}
	if _, e = client.New(cfg); e != nil {
		t.Fatal(e)
	}
	if e = generate(dir, "", "test", 19001, false); e == nil {
		t.Fatal("overwrote existing credentials")
	}
}

func TestRelayLabBindsStatusToAssigneeAndGateway(t *testing.T) {
	dir := t.TempDir()
	if err := generateRelayOptions(dir, "/state", "relay", 19001, true, true, true, false, true); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"node-1", "node-2", "node-3"} {
		var cfg struct {
			PeerGrant authority.SignedGrant `json:"peer_grant"`
			Relay     struct {
				Path   string `json:"path"`
				Routes map[string]struct {
					Assignee        string `json:"assignee"`
					StatusPrincipal string `json:"status_principal"`
				} `json:"routes"`
			} `json:"task_relay"`
		}
		data, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		route := cfg.Relay.Routes["node-1"]
		if cfg.Relay.Path != "/state/"+name+".task-relay" || route.Assignee != "sim-robot/navigation" || route.StatusPrincipal != "lab/node-1" {
			t.Fatal(cfg.Relay)
		}
		g := cfg.PeerGrant.Grant
		status := false
		for _, scope := range g.Scopes {
			if scope == "task-status" {
				status = true
			}
		}
		if status != (i == 0) {
			t.Fatal("status authority on wrong node", name)
		}
		if i == 0 && (g.TaskStatusAssignee != route.Assignee || g.TaskStatusAudience != name) {
			t.Fatal(g)
		}
	}
	for i, name := range []string{"operator-1.json", "operator-2.json", "operator-3.json"} {
		var cfg client.Config
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		want := []string{"node-1", "node-2", "node-3"}[i]
		if cfg.Audience != want || cfg.URL != "http://world-"+want+":8080" || cfg.Grant.Grant.Kind != "operator" {
			t.Fatal(cfg.Audience, cfg.URL)
		}
	}
	if err := generateRelayOptions(t.TempDir(), "", "relay", 19001, false, false, false, false, true); err == nil {
		t.Fatal("relay without mission authority")
	}
}

func TestReplayLabSeparatesSourcesAndExcludesMissions(t *testing.T) {
	dir := t.TempDir()
	if err := generateModeOptions(dir, "", "evaluation", 19001, false, false, true, true); err != nil {
		t.Fatal(err)
	}
	var sensor client.Config
	b, err := os.ReadFile(filepath.Join(dir, "sensor-camera-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &sensor); err != nil {
		t.Fatal(err)
	}
	g := sensor.Grant.Grant
	if g.Source.Device != "sim-replay-camera-a" || g.Source.Service != "camera-a" || g.Source.Stream != "replays" || g.Domain != "simulation" || g.PublishAudience != "node-1" {
		t.Fatal(g)
	}
	if _, err = os.Stat(filepath.Join(dir, "operator-1.json")); !os.IsNotExist(err) {
		t.Fatal("replay lab issued motion credentials")
	}
	var fusion struct {
		Sources map[string]struct{ Device, Stream string } `json:"sources"`
	}
	b, err = os.ReadFile(filepath.Join(dir, "fusion.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &fusion); err != nil {
		t.Fatal(err)
	}
	if fusion.Sources["camera-a"].Device != g.Source.Device || fusion.Sources["camera-a"].Stream != g.Source.Stream {
		t.Fatal("fusion bound to original sensors")
	}
	if err := generateModeOptions(t.TempDir(), "", "evaluation", 19001, false, true, true, true); err == nil {
		t.Fatal("replay enabled missions")
	}
}
