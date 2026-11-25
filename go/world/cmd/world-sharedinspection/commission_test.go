package main

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/executor"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func inputFixture(t *testing.T) (commissionInput, []byte, time.Time) {
	t.Helper()
	raw, e := os.ReadFile("../../examples/collaborative-office/scene.json")
	if e != nil {
		t.Fatal(e)
	}
	model := strings.Repeat("b", 64)
	return commissionInput{Version: 1, Tenant: "tenant-actual", Device: "spiffe://wendy.sh/tenant/tenant-actual/device/a", Site: "studio-01", RunID: "shared-mission-test", Frame: world.FrameRef{ID: "office-floor", Revision: "shared-office-" + model[:16]}, PackageSHA256: strings.Repeat("a", 64), ModelSHA256: model, OfficeSHA256: hash(raw), SceneURL: "http://10.20.0.7:8091", GatewayURL: "http://10.20.0.8:8093"}, raw, time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
}

func TestCommissionActualScopeSignedContextAndSeparateRoles(t *testing.T) {
	in, office, now := inputFixture(t)
	files, e := commission(in, office, now)
	if e != nil {
		t.Fatal(e)
	}
	c := files["mission-context.json"].(missioncontrol.SignedContext)
	gateway := files["gateway.json"].(map[string]any)
	root := gateway["root_public_key"].(ed25519.PublicKey)
	if e = authority.VerifyMissionControlContext(c, root); e != nil {
		t.Fatal(e)
	}
	if c.Context.Tenant != in.Tenant || c.Context.Site != in.Site || c.Context.Frame != in.Frame || c.Context.RunID != in.RunID || c.Context.PackageSHA256 != in.PackageSHA256 || c.Context.Assignee != assignee {
		t.Fatal("observed binding changed")
	}
	if c.Context.Area.Min[1] > .24 || c.Context.Area.Max[1] < .24 || c.Context.SpeedMPS > .5 {
		t.Fatal("planar envelope differs")
	}
	v, e := authority.NewVerifier(authority.Config{RootPublicKey: root, Tenant: in.Tenant, Site: in.Site, Domain: "simulation", Audience: audience, Now: func() time.Time { return now }, Replay: authority.NewMemoryReplay(100)})
	if e != nil {
		t.Fatal(e)
	}
	if e = v.UpdateRevocations(gateway["revocations"].(authority.SignedRevocations)); e != nil {
		t.Fatal(e)
	}
	for role, operation := range map[string]string{"operator": "mission", "proposer": "propose", "executor": "execute", "evidence-reader": "evidence-read"} {
		cfg := files[role+".json"].(client.Config)
		if role == "operator" && !reflect.DeepEqual(cfg.Grant.Grant.Scopes, []string{"mission", "stop", "read"}) {
			t.Fatal("native operator credential contract changed")
		}
		if cfg.Grant.Grant.Source.Device != in.Device || cfg.URL != in.GatewayURL {
			t.Fatal("source or observed endpoint changed")
		}
		if _, e = v.VerifyGrant(cfg.Grant, operation); e != nil {
			t.Fatal(role, e)
		}
		if _, e = client.New(cfg); e != nil {
			t.Fatal(e)
		}
		if role != "operator" {
			if _, e = v.VerifyGrant(cfg.Grant, "mission"); e == nil {
				t.Fatal("nonoperator can issue MOVE")
			}
		}
	}
	reader := files["evidence-reader.json"].(client.Config)
	operator := files["operator.json"].(client.Config)
	if !reflect.DeepEqual(reader.Grant.Grant.Scopes, []string{"read", "evidence-read"}) ||
		reader.Grant.Grant.Principal == operator.Grant.Grant.Principal ||
		reflect.DeepEqual(reader.PrivateKey, operator.PrivateKey) {
		t.Fatal("evidence viewer must have a separate read-only credential")
	}
	if _, e = v.VerifyGrant(reader.Grant, "read"); e != nil {
		t.Fatal(e)
	}
	for _, operation := range []string{"mission", "stop", "execute", "propose", "evidence-write", "evidence-pin"} {
		if _, e = v.VerifyGrant(reader.Grant, operation); e == nil {
			t.Fatal("evidence viewer gained control capability", operation)
		}
	}
	planar := files["planar.json"].(executor.PlanarConfig)
	var original officeDescription
	_ = json.Unmarshal(office, &original)
	if !reflect.DeepEqual(planar.Grid, original.Grid) || planar.RobotID != "atlas" || planar.FloorY != .24 || len(planar.Reservations) != 2 {
		t.Fatal("existing office navigation changed")
	}
	if files["ui.json"] != nil || files["mission.json"] != nil {
		t.Fatal("implicit endpoint or motion task generated")
	}
	if files["public.json"].(map[string]any)["mission_signed"] != false {
		t.Fatal("claimed motion")
	}
}

func TestCommissionRefusesMismatchedSceneAndUsesExplicitOperatorEndpoint(t *testing.T) {
	for _, field := range []string{"frame", "hash", "site", "url"} {
		t.Run(field, func(t *testing.T) {
			in, raw, now := inputFixture(t)
			switch field {
			case "frame":
				in.Frame.Revision = "old"
			case "hash":
				in.OfficeSHA256 = strings.Repeat("0", 64)
			case "site":
				in.Site = "other"
			case "url":
				in.SceneURL = "http://localhost"
			}
			if _, e := commission(in, raw, now); e == nil {
				t.Fatal("mismatch accepted")
			}
		})
	}
	in, raw, now := inputFixture(t)
	in.OperatorURL = "http://127.0.0.1:38193"
	files, e := commission(in, raw, now)
	if e != nil {
		t.Fatal(e)
	}
	if files["operator.json"].(client.Config).URL != in.OperatorURL || files["evidence-reader.json"].(client.Config).URL != in.OperatorURL || files["proposer.json"].(client.Config).URL != in.GatewayURL || files["executor.json"].(client.Config).URL != in.GatewayURL {
		t.Fatal("operator endpoint leaked into in-node clients")
	}
	directory := t.TempDir()
	config := uiConfig(directory)
	if config["evidence_credentials_file"] != filepath.Join(directory, "evidence-reader.json") {
		t.Fatal("evidence viewer path missing")
	}
	for key, value := range config {
		if strings.HasSuffix(key, "_file") || key == "journal_directory" {
			if !filepath.IsAbs(value.(string)) {
				t.Fatal(key)
			}
		}
	}
}

func TestExecutorBindingAndExclusivePrivateOutput(t *testing.T) {
	in, _, _ := inputFixture(t)
	args, e := executorArgs(launchConfig{1, in.SceneURL, in.RunID, in.Frame})
	if e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(args, " ")
	for _, part := range []string{"--adapter " + in.SceneURL, "--adapter-read-token-file /config/scene-read-token", "--adapter-token-file /config/scene-control-token", "--revision " + in.Frame.Revision, "--planar-config /config/planar.json"} {
		if !strings.Contains(joined, part) {
			t.Fatal(part)
		}
	}
	path := filepath.Join(t.TempDir(), "handoff.json")
	if e = writeNew(path, map[string]any{"version": 1}); e != nil {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(path)
	if e = writeNew(path, map[string]any{"version": 2}); e == nil {
		t.Fatal("overwritten")
	}
	after, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if string(before) != string(after) || st.Mode().Perm() != 0600 {
		t.Fatal("private exclusive write changed")
	}
}
