package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/incident"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestInspectionSetupCommissionedAuthorities(t *testing.T) {
	_, root, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var cfg incident.Config
	cfg.Gas.Tenant = "lab"
	cfg.Visual.Site = "lab"
	cfg.Visual.Frame = world.FrameRef{ID: "floor", Revision: "1"}
	cfg.Visual.Package = strings.Repeat("a", 64)
	cfg.Visual.Simulation.RunID = "setup-test"
	files := map[string][]byte{}
	extras, err := inspectionSetup(t.TempDir(), cfg, 19580, root, time.Now(), time.Now().Add(time.Hour), func(name string, v any) error { b, e := json.Marshal(v); files[name] = b; return e })
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"proposer", "operator", "executor", "observer", "evidence-reader"} {
		var c client.Config
		if err := json.Unmarshal(files["inspection/"+role+".json"], &c); err != nil {
			t.Fatal(role, err)
		}
		if _, err := client.New(c); err != nil {
			t.Fatal(role, err)
		}
		if role == "executor" && (c.Grant.Grant.PublishAudience != "node-2" || c.Grant.Grant.ExecuteAssignee != "atlas/navigation") {
			t.Fatal("executor authority not audience-bound")
		}
		if role == "proposer" && strings.Join(c.Grant.Grant.Scopes, ",") != "propose,read" {
			t.Fatal("proposer gained execution authority")
		}
	}
	if extras["mission_clock"] == nil || extras["tasks"] == nil || extras["evidence"] == nil {
		t.Fatal("missing commissioned services")
	}
}
