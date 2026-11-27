package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/collabdemo"
	"github.com/wendylabsinc/wendy/go/world/internal/executor"
	"github.com/wendylabsinc/wendy/go/world/internal/incident"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missionclock"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"path/filepath"
	"time"
)

// Uses the existing simulation grant, task, clock, evidence and planar executor
// contracts. Separate operator credentials are never passed to a reader app.
func inspectionSetup(root string, cfg incident.Config, base int, key ed25519.PrivateKey, now, until time.Time, write func(string, any) error) (map[string]any, error) {
	run := cfg.Visual.Simulation.RunID
	dir := filepath.Join(root, "inspection")
	audience := "node-2"
	assignee := "atlas/navigation"
	grant := func(role, kind string, scopes []string) (client.Config, error) {
		p, k, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return client.Config{}, e
		}
		g := authority.Grant{Version: 1, ID: "incidentinspection/" + run + "/" + role, Tenant: cfg.Gas.Tenant, Site: cfg.Visual.Site, Domain: "simulation", Principal: "incidentinspection/" + role, Kind: kind, Source: world.Source{Device: "incident-inspection", App: role, Service: role, Stream: "state"}, PublicKey: p, Scopes: scopes, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: until.UnixMilli()}
		if role == "mission-clock" {
			g.PublishAudience = audience
		}
		if role == "executor" {
			g.ExecuteAssignee = assignee
			g.PublishAudience = audience
		}
		signed, e := authority.SignGrant(g, key)
		return client.Config{URL: fmt.Sprintf("http://127.0.0.1:%d", base+1), Audience: audience, Grant: signed, PrivateKey: k, AllowInsecureSimulation: true}, e
	}
	planar := executor.PlanarConfig{RobotID: "atlas", FloorY: .24, Grid: collabdemo.Grid{Origin: collabdemo.Vec{-3, -2.2}, Resolution: .1, Width: 60, Height: 44, Blocked: []collabdemo.Cell{}}}
	// The eight fixed sensor posts are expanded by the simulated robot radius.
	for y := 0; y < planar.Grid.Height; y++ {
		for x := 0; x < planar.Grid.Width; x++ {
			px := -3 + (float64(x)+.5)*.1
			pz := -2.2 + (float64(y)+.5)*.1
			for _, sensor := range cfg.Gas.Field.Sensors {
				dx := px - sensor.Position[0]
				dz := pz + sensor.Position[1]
				if dx*dx+dz*dz < .4*.4 {
					planar.Grid.Blocked = append(planar.Grid.Blocked, collabdemo.Cell{x, y})
					break
				}
			}
		}
	}
	digest, e := executor.PlanarConfigSHA256(planar)
	if e != nil {
		return nil, e
	}
	context, e := authority.SignMissionControlContext(missioncontrol.Context{Version: 1, Tenant: cfg.Gas.Tenant, Site: cfg.Visual.Site, Audience: audience, RunID: run, PackageSHA256: cfg.Visual.Package, Assignee: assignee, Frame: cfg.Visual.Frame, Type: missioncontrol.InspectType, Class: mission.Move, Mode: mission.Autonomous, Area: missioncontrol.Bounds{Min: [3]float64{-3, .23, -2.2}, Max: [3]float64{3, .25, 2.2}}, RequiredSensors: []string{"gas", "local-obstacles"}, MinimumEnergy: .2, Fallback: mission.Hold, SpeedMPS: .35, ToleranceM: .18, StopSpeedMPS: .03, MaxDurationMS: 60000, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: until.UnixMilli()}, key)
	if e != nil {
		return nil, e
	}
	clock, e := grant("mission-clock", "device", []string{"mission-clock-server"})
	if e != nil {
		return nil, e
	}
	pins := missionclock.Pins{RootPublicKey: key.Public().(ed25519.PublicKey), GatewayPrincipal: clock.Grant.Grant.Principal, GatewayKeySHA256: authority.KeyID(clock.Grant.Grant.PublicKey), RunID: run, ClockDomain: "simulation:" + run}
	files := map[string]any{"planar.json": planar, "mission-context.json": context, "mission-clock-trust.json": pins, "inspection-destination.json": missioncontrol.InspectionDestination{Version: 1, Mode: "planar_nearby", FloorY: .24, MaxOffsetM: 1, PlanarConfigSHA256: digest}, "executor-launch.json": map[string]any{"version": 1, "scene_url": fmt.Sprintf("http://127.0.0.1:%d", base+6), "run_id": run, "frame": cfg.Visual.Frame}}
	for _, role := range []struct {
		name, kind string
		scopes     []string
	}{{"proposer", "agent", []string{"propose", "read"}}, {"operator", "operator", []string{"mission", "stop", "read"}}, {"executor", "app", []string{"execute", "read", "evidence-read", "evidence-write"}}, {"evidence-reader", "app", []string{"read", "evidence-read"}}} {
		c, e := grant(role.name, role.kind, role.scopes)
		if e != nil {
			return nil, e
		}
		files[role.name+".json"] = c
		if role.name == "operator" {
			west := c
			west.URL = fmt.Sprintf("http://127.0.0.1:%d", base+2)
			west.Audience = "node-3"
			files["observer.json"] = west
		}
	}
	for name, value := range files {
		if e := write("inspection/"+name, value); e != nil {
			return nil, e
		}
	}
	ui := map[string]any{"version": 1, "profile": map[string]any{"kind": "planar", "floor_y": .24}, "credentials_file": filepath.Join(dir, "operator.json"), "context_file": filepath.Join(dir, "mission-context.json"), "clock_pins_file": filepath.Join(dir, "mission-clock-trust.json"), "journal_directory": filepath.Join(dir, "journal"), "handoff_file": filepath.Join(dir, "handoff.json"), "evidence_credentials_file": filepath.Join(dir, "evidence-reader.json")}
	if e := write("inspection/ui.json", ui); e != nil {
		return nil, e
	}
	return map[string]any{"mission_clock": map[string]any{"grant": clock.Grant, "private_key": clock.PrivateKey}, "tasks": map[string]any{"path": filepath.Join(dir, "tasks.json"), "assignee": assignee, "executor_principal": "incidentinspection/executor", "catalog": map[string]string{missioncontrol.InspectType: "MOVE"}, "control_context": context, "enable_supervision": false, "max_lease_ms": 500, "max_telemetry_age_ms": 500, "max_tasks": 32}, "proposals": map[string]any{"path": filepath.Join(dir, "proposals.json"), "max_entries": 64, "max_bytes": 4 << 20}, "evidence": map[string]any{"path": filepath.Join(dir, "evidence"), "quota_bytes": 64 << 20, "max_blob_bytes": 8 << 20}}, nil
}
