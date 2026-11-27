package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/collabdemo"
	"github.com/wendylabsinc/wendy/go/world/internal/executor"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missionclock"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const audience = "atlas-mission-gateway"
const assignee = "atlas/navigation"

type commissionInput struct {
	Version       int            `json:"version"`
	Tenant        string         `json:"tenant"`
	Device        string         `json:"device"`
	Site          string         `json:"site"`
	RunID         string         `json:"run_id"`
	Frame         world.FrameRef `json:"frame"`
	PackageSHA256 string         `json:"package_sha256"`
	ModelSHA256   string         `json:"model_sha256"`
	OfficeSHA256  string         `json:"office_scene_sha256"`
	SceneURL      string         `json:"scene_url"`
	GatewayURL    string         `json:"gateway_url"`
	OperatorURL   string         `json:"operator_url,omitempty"`
	ViewerURL     string         `json:"viewer_url,omitempty"`
}

type officeDescription struct {
	Schema string          `json:"schema_version"`
	Grid   collabdemo.Grid `json:"grid"`
	Robots []struct {
		ID    string         `json:"id"`
		Start collabdemo.Vec `json:"start"`
	} `json:"robots"`
}

type launchConfig struct {
	Version  int            `json:"version"`
	SceneURL string         `json:"scene_url"`
	RunID    string         `json:"run_id"`
	Frame    world.FrameRef `json:"frame"`
}

func hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func digest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}
func httpOrigin(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "http" && u.Hostname() != "" && u.Port() != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}
func text(s string) bool {
	return s != "" && len(s) <= 256 && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}

// commission creates the existing simulation-only authority format. It neither
// imports the native Agent's keys nor admits or signs a motion instruction.
func commission(input commissionInput, officeRaw []byte, now time.Time) (map[string]any, error) {
	if input.Version != 1 || !text(input.Tenant) || !text(input.Device) || input.Site != "studio-01" || !text(input.RunID) || strings.ContainsAny(input.RunID, " /\\") || len(input.RunID) > 80 || input.Frame.ID != "office-floor" || !digest(input.PackageSHA256) || !digest(input.ModelSHA256) || input.Frame.Revision != "shared-office-"+input.ModelSHA256[:16] || hash(officeRaw) != input.OfficeSHA256 || !httpOrigin(input.SceneURL) || !httpOrigin(input.GatewayURL) || (input.OperatorURL != "" && !httpOrigin(input.OperatorURL)) {
		return nil, errors.New("exact observed shared-office commissioning input required")
	}
	if input.ViewerURL != "" && !httpOrigin(input.ViewerURL) {
		return nil, errors.New("explicit viewer HTTP origin required")
	}
	var office officeDescription
	if json.Unmarshal(officeRaw, &office) != nil || office.Schema != "wendy.collaborative-office.scene.v1" {
		return nil, errors.New("existing office metadata required")
	}
	planar := executor.PlanarConfig{RobotID: "atlas", FloorY: .24, Grid: office.Grid}
	atlas := false
	for _, r := range office.Robots {
		if r.ID == "atlas" {
			atlas = true
		} else {
			planar.Reservations = append(planar.Reservations, []collabdemo.Vec{r.Start})
		}
	}
	if !atlas {
		return nil, errors.New("Atlas absent from office")
	}
	if _, e := executor.NewPlanarNavigation(planar); e != nil {
		return nil, e
	}
	planarSHA, e := executor.PlanarConfigSHA256(planar)
	if e != nil {
		return nil, e
	}
	destination := missioncontrol.InspectionDestination{Version: 1, Mode: "planar_nearby", FloorY: planar.FloorY, MaxOffsetM: 2, PlanarConfigSHA256: planarSHA}
	root, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return nil, e
	}
	until := now.Add(59 * time.Minute)
	grant := func(role, kind string, scopes []string) (authority.SignedGrant, ed25519.PrivateKey, error) {
		pub, private, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return authority.SignedGrant{}, nil, e
		}
		g := authority.Grant{Version: 1, ID: "sharedinspection/" + input.RunID + "/" + role, Tenant: input.Tenant, Site: input.Site, Domain: "simulation", Principal: "sharedinspection/" + role, Kind: kind, Source: world.Source{Device: input.Device, App: "wendy-world-missions", Service: role, Stream: "tasks"}, PublicKey: pub, Scopes: scopes, PublishAudience: audience, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: until.UnixMilli()}
		if role == "executor" {
			g.ExecuteAssignee = assignee
		}
		if role == "gateway" {
			g.TaskStatusAssignee = assignee
			g.TaskStatusAudience = audience
		}
		signed, e := authority.SignGrant(g, key)
		return signed, private, e
	}
	rev, e := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: input.Tenant, Site: input.Site, Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: until.UnixMilli()}, key)
	if e != nil {
		return nil, e
	}
	g := office.Grid
	context, e := authority.SignMissionControlContext(missioncontrol.Context{Version: 1, Tenant: input.Tenant, Site: input.Site, Audience: audience, RunID: input.RunID, PackageSHA256: input.PackageSHA256, Assignee: assignee, Frame: input.Frame, Type: missioncontrol.InspectType, Class: mission.Move, Mode: mission.Autonomous, Area: missioncontrol.Bounds{Min: [3]float64{g.Origin[0], .23, -(g.Origin[1] + float64(g.Height)*g.Resolution)}, Max: [3]float64{g.Origin[0] + float64(g.Width)*g.Resolution, .25, -g.Origin[1]}}, RequiredSensors: []string{"gas", "local-obstacles"}, MinimumEnergy: .2, Fallback: mission.Hold, SpeedMPS: .35, ToleranceM: .4, StopSpeedMPS: .03, MaxDurationMS: 60000, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: until.UnixMilli()}, key)
	if e != nil {
		return nil, e
	}
	clockGrant, clockKey, e := grant("mission-clock", "device", []string{"mission-clock-server"})
	if e != nil {
		return nil, e
	}
	pins := missionclock.Pins{RootPublicKey: root, GatewayPrincipal: clockGrant.Grant.Principal, GatewayKeySHA256: authority.KeyID(clockGrant.Grant.PublicKey), RunID: input.RunID, ClockDomain: "simulation:" + input.RunID}
	gatewayGrant, gatewayKey, e := grant("gateway", "device", []string{"read", "replicate", "task-status"})
	if e != nil {
		return nil, e
	}
	files := map[string]any{
		"mission-context.json": context, "mission-clock-trust.json": pins, "planar.json": planar,
		"inspection-destination.json": destination,
		"executor-launch.json":        launchConfig{Version: 1, SceneURL: input.SceneURL, RunID: input.RunID, Frame: input.Frame},
		"gateway.json": map[string]any{"allow_insecure_simulation": true, "listen": ":8093", "data_path": "/state/world.json", "proof_path": "/state/proofs.json", "replay_path": "/state/replay.json", "revocation_path": "/state/revocations.json", "tenant": input.Tenant, "site": input.Site, "clock_domain": "simulation:" + input.RunID, "audience": audience, "frames": []world.FrameRef{input.Frame}, "root_public_key": root, "revocations": rev, "peer_grant": gatewayGrant, "peer_private_key": gatewayKey, "peers": []any{}, "sync_interval_ms": 500, "max_records": 1024,
			"mission_clock": map[string]any{"grant": clockGrant, "private_key": clockKey},
			"tasks":         map[string]any{"path": "/state/tasks.json", "assignee": assignee, "executor_principal": "sharedinspection/executor", "catalog": map[string]string{missioncontrol.InspectType: "MOVE"}, "control_context": context, "enable_supervision": false, "max_lease_ms": 500, "max_telemetry_age_ms": 500, "max_tasks": 32},
			"proposals":     map[string]any{"path": "/state/proposals.json", "max_entries": 64, "max_bytes": 4 << 20},
			"evidence":      map[string]any{"path": "/state/evidence", "quota_bytes": 64 << 20, "max_blob_bytes": 8 << 20}},
	}
	for _, role := range []struct {
		name, kind string
		scopes     []string
	}{
		{"executor", "app", []string{"execute", "read", "evidence-write", "evidence-read"}},
		{"operator", "operator", []string{"mission", "stop", "read"}},
		{"evidence-reader", "app", []string{"read", "evidence-read"}},
		{"proposer", "agent", []string{"propose", "read"}},
	} {
		g, k, e := grant(role.name, role.kind, role.scopes)
		if e != nil {
			return nil, e
		}
		endpoint := input.GatewayURL
		if (role.name == "operator" || role.name == "evidence-reader") && input.OperatorURL != "" {
			endpoint = input.OperatorURL
		}
		files[role.name+".json"] = client.Config{URL: endpoint, Audience: audience, Grant: g, PrivateKey: k, AllowInsecureSimulation: true}
	}
	files["public.json"] = map[string]any{"version": 1, "tenant": input.Tenant, "site": input.Site, "device": input.Device, "run_id": input.RunID, "frame": input.Frame, "package_sha256": input.PackageSHA256, "model_sha256": input.ModelSHA256, "office_scene_sha256": input.OfficeSHA256, "assignee": assignee, "floor_y": .24, "root_public_key": root, "context_sha256": missioncontrol.Digest(context), "gateway_url": input.GatewayURL, "operator_endpoint_configured": input.OperatorURL != "", "simulation_only": true, "mission_signed": false}
	if input.ViewerURL != "" {
		public, profile, err := commissionViewer(input, now)
		if err != nil {
			return nil, err
		}
		files["viewer-public.json"], files["viewer-profile.json"] = public, profile
	}
	return files, nil
}

func uiConfig(out string) map[string]any {
	return map[string]any{"version": 1, "profile": map[string]any{"kind": "planar", "floor_y": .24}, "credentials_file": filepath.Join(out, "operator.json"), "evidence_credentials_file": filepath.Join(out, "evidence-reader.json"), "context_file": filepath.Join(out, "mission-context.json"), "clock_pins_file": filepath.Join(out, "mission-clock-trust.json"), "journal_directory": filepath.Join(out, "journal"), "handoff_file": filepath.Join(out, "handoff.json")}
}
