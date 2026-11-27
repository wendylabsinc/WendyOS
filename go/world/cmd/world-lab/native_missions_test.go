package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/missionclock"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/worldpackage"
)

func TestNativeMissionLabSeparatesKeysAndBindsVerifiedPackage(t *testing.T) {
	dir := t.TempDir()
	path := "../../examples/live-office/world.json"
	if err := generateNativeMissionOptions(dir, "/lab", "native-missions", 19001, true, true, true, false, true, true, path); err != nil {
		t.Fatal(err)
	}
	read := func(name string, out any) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatal(err)
		}
	}
	var bound missioncontrol.SignedContext
	var pins missionclock.Pins
	read("mission-context.json", &bound)
	read("mission-clock-trust.json", &pins)
	if err := authority.VerifyMissionControlContext(bound, pins.RootPublicKey); err != nil {
		t.Fatal(err)
	}
	bundle, err := worldpackage.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Context.PackageSHA256 != bundle.Manifest.SHA256 || bound.Context.RunID != "native-missions" || pins.ClockDomain != "simulation:native-missions" {
		t.Fatal("wrong commissioning scope")
	}
	var node struct {
		MissionClock struct {
			Grant authority.SignedGrant `json:"grant"`
		} `json:"mission_clock"`
		Tasks struct {
			Context missioncontrol.SignedContext `json:"control_context"`
		} `json:"tasks"`
	}
	read("node-1.json", &node)
	if !missioncontrol.Equal(node.Tasks.Context, bound) || !reflect.DeepEqual(node.MissionClock.Grant.Grant.Scopes, []string{"mission-clock-server"}) || authority.KeyID(node.MissionClock.Grant.Grant.PublicKey) != pins.GatewayKeySHA256 {
		t.Fatal("node did not receive the exact context and separate clock key")
	}
	keys := map[string]bool{pins.GatewayKeySHA256: true}
	for _, platform := range []string{"macos", "ios", "visionos", "android"} {
		var cfg, readOnly, expired client.Config
		read("mission-"+platform+".json", &cfg)
		read("contributor-"+platform+".json", &readOnly)
		read("mission-expired-"+platform+".json", &expired)
		key := authority.KeyID(cfg.Grant.Grant.PublicKey)
		if keys[key] || key == authority.KeyID(readOnly.Grant.Grant.PublicKey) || cfg.ClockTrust != nil || cfg.Audience != "node-1" || cfg.Grant.Grant.PublishAudience != "node-1" || cfg.Grant.Grant.Kind != "operator" {
			t.Fatal("mixed credential custody")
		}
		keys[key] = true
		if expired.Grant.Grant.NotAfterUnixMS >= time.Now().UnixMilli() || expired.Grant.Grant.Principal != cfg.Grant.Grant.Principal || expired.Grant.Grant.ID == cfg.Grant.Grant.ID {
			t.Fatal("expired stop fixture is not distinct")
		}
	}
}

func TestNativeMissionPackageAndModeAreExplicit(t *testing.T) {
	if err := generateNativeMissionOptions(t.TempDir(), "", "native", 19001, false, true, true, false, false, false, "../../examples/live-office/world.json"); err == nil {
		t.Fatal("missing proposals accepted")
	}
	if _, err := nativeMissionManifest("../../examples/articulated-twin/world.json"); err == nil {
		t.Fatal("generic articulated model accepted as one-axis commissioned robot")
	}
}
