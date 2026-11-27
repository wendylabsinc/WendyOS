package main

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missionclock"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/internal/worldpackage"
)

func nativeMissionManifest(path string) (*worldpackage.Manifest, error) {
	if path == "" {
		return nil, nil
	}
	b, err := worldpackage.Load(path)
	if err != nil {
		return nil, err
	}
	var format struct {
		Schema     string `json:"schema_version"`
		Provenance string `json:"provenance"`
	}
	if err := json.Unmarshal(b.Root, &format); err != nil {
		return nil, err
	}
	if format.Schema != "wendy.world.package.v0" || format.Provenance != "synthetic" || b.Manifest.Site != "synthetic-office" || b.Manifest.FrameRevision != "office-frame-v1" {
		return nil, errors.New("native mission fixture requires the synthetic office one-axis package and frame")
	}
	return &b.Manifest, nil
}

type nativeMissionSetup struct {
	context missioncontrol.SignedContext
	pins    missionclock.Pins
}

func newNativeMissionSetup(manifest *worldpackage.Manifest, runID string, key ed25519.PrivateKey, now, until time.Time) (*nativeMissionSetup, error) {
	if manifest == nil {
		return nil, nil
	}
	c := missioncontrol.Context{
		Version: 1, Tenant: "lab", Site: manifest.Site, Audience: "node-1", RunID: runID,
		PackageSHA256: manifest.SHA256, Assignee: "sim-robot/navigation",
		Frame: world.FrameRef{ID: "office-floor", Revision: manifest.FrameRevision},
		Type:  missioncontrol.InspectType, Class: mission.Move, Mode: mission.Autonomous,
		Area:            missioncontrol.Bounds{Min: [3]float64{-2.1, .1, -.1}, Max: [3]float64{2.1, .3, .1}},
		RequiredSensors: []string{"gas", "local-obstacles"}, MinimumEnergy: .2, Fallback: mission.Hold,
		SpeedMPS: .1, ToleranceM: .03, StopSpeedMPS: .03, MaxDurationMS: 120000,
		NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: until.UnixMilli(),
	}
	signed, err := authority.SignMissionControlContext(c, key)
	if err != nil {
		return nil, err
	}
	return &nativeMissionSetup{context: signed}, nil
}

func (n *nativeMissionSetup) configure(cfg map[string]any, grant authority.SignedGrant, key ed25519.PrivateKey, root ed25519.PublicKey) {
	cfg["mission_clock"] = map[string]any{"grant": grant, "private_key": []byte(key)}
	cfg["tasks"].(map[string]any)["control_context"] = n.context
	n.pins = missionclock.Pins{RootPublicKey: []byte(root), GatewayPrincipal: grant.Grant.Principal,
		GatewayKeySHA256: authority.KeyID(grant.Grant.PublicKey), RunID: n.context.Context.RunID,
		ClockDomain: "simulation:" + n.context.Context.RunID}
}

func (n *nativeMissionSetup) write(write func(string, any) error, grant func(string, string, []string) (authority.SignedGrant, ed25519.PrivateKey, error), root ed25519.PrivateKey, basePort int, container bool, now time.Time) error {
	if err := write("mission-context.json", n.context); err != nil {
		return err
	}
	if err := write("mission-clock-trust.json", n.pins); err != nil {
		return err
	}
	host, port := "127.0.0.1", basePort
	if container {
		host, port = "world-node-1", 8080
	}
	for _, platform := range []string{"macos", "ios", "visionos", "android"} {
		g, key, err := grant("sim-native-"+platform, "operator", []string{"mission", "stop", "read"})
		if err != nil {
			return err
		}
		cfg := client.Config{URL: fmt.Sprintf("http://%s:%d", host, port), Audience: "node-1", Grant: g, PrivateKey: []byte(key), AllowInsecureSimulation: true}
		if err := write("mission-"+platform+".json", cfg); err != nil {
			return err
		}
		// A distinct, explicitly named fixture demonstrates that expired roles
		// can still stop. It never grants mission authority at the current time.
		expired := g.Grant
		expired.ID += "/expired-stop-test"
		expired.NotBeforeUnixMS, expired.NotAfterUnixMS = now.Add(-2*time.Minute).UnixMilli(), now.Add(-time.Minute).UnixMilli()
		cfg.Grant, err = authority.SignGrant(expired, root)
		if err != nil {
			return err
		}
		if err := write("mission-expired-"+platform+".json", cfg); err != nil {
			return err
		}
	}
	return nil
}
