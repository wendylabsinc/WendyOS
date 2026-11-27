package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
)

func TestProposalLabKeepsAgentSeparateFromMissionAuthority(t *testing.T) {
	dir := t.TempDir()
	if err := generateProposalOptions(dir, "/state", "proposal-test", 19001, true, true, false, false, false, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "agent-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg client.Config
	if err = authority.DecodeStrict(data, &cfg); err != nil {
		t.Fatal(err)
	}
	g := cfg.Grant.Grant
	if g.Kind != "agent" || g.Domain != "simulation" || cfg.Audience != "node-1" || cfg.URL != "http://world-node-1:8080" || !reflect.DeepEqual(g.Scopes, []string{"read", "propose"}) {
		t.Fatal("unexpected agent authority")
	}
	if _, err = client.New(cfg); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"node-1", "node-2", "node-3"} {
		data, err = os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var node map[string]json.RawMessage
		if err = json.Unmarshal(data, &node); err != nil {
			t.Fatal(err)
		}
		if (node["proposals"] != nil) != (name == "node-1") {
			t.Fatal("proposal store assigned to wrong node")
		}
	}
	if err = generateProposalOptions(t.TempDir(), "", "bad", 19001, false, false, false, false, false, true); err == nil {
		t.Fatal("proposal fixture lacks trusted catalog")
	}
}
