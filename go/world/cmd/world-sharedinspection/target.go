package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/collabdemo"
	"github.com/wendylabsinc/wendy/go/world/internal/executor"
	"github.com/wendylabsinc/wendy/go/world/internal/missionclock"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

// This sentinel is used only before any proposal POST. It is a routine planning
// refusal, not an acknowledgment or an unknown mutation outcome.
var errInspectionUnavailable = errors.New("inspection unavailable before proposal submission")

type navigationReview struct {
	Version           int          `json:"version"`
	RobotID           string       `json:"robot_id"`
	StartPosition     [3]float64   `json:"start_position_m"`
	StateBootID       string       `json:"state_boot_id"`
	StateSession      string       `json:"state_session"`
	StateSequence     uint64       `json:"state_sequence"`
	StateSampleUnixMS int64        `json:"state_sample_unix_ms"`
	SimulationTimeS   float64      `json:"simulation_time_s"`
	Route             [][3]float64 `json:"route_world"`
	OffsetM           float64      `json:"offset_m"`
	EstimateBasis     string       `json:"estimate_basis"`
}

type navigationPlan struct {
	Target                             [3]float64
	Descriptor                         missioncontrol.InspectionDestination
	LengthM                            float64
	NominalTravelMS, MinimumDurationMS int64
	Review                             navigationReview
}

func resolveNavigation(input missionInput, state executor.State, planar executor.PlanarConfig,
	descriptor missioncontrol.InspectionDestination, policy missioncontrol.Context, now time.Time) (*navigationPlan, error) {
	sha, err := executor.PlanarConfigSHA256(planar)
	if err != nil || descriptor.Validate() != nil || descriptor.PlanarConfigSHA256 != sha || descriptor.FloorY != planar.FloorY || planar.RobotID != "atlas" {
		return nil, errors.New("commissioned planar configuration differs")
	}
	if policy.Validate() != nil || policy.Assignee != assignee || (policy.Audience != audience && !(policy.Audience == "node-2" && (input.ResultSchema == missioncontrol.IncidentInspectionType || input.ResultSchema == missioncontrol.RisingInspectionType))) {
		return nil, errors.New("commissioned inspection context required")
	}
	var trigger tasking.TriggerRef
	if authority.DecodeStrictLimit(input.TriggerJSON, &trigger, 512<<10) != nil || trigger.SHA256 != input.TriggerSHA256 ||
		trigger.Validate(policy.Site, policy.Frame) != nil || trigger.Component.Key.Tenant != policy.Tenant ||
		trigger.Component.Timing.ClockDomain != "simulation:"+policy.RunID {
		return nil, errors.New("navigation observation differs from commissioned context")
	}
	observed, err := missioncontrol.NativeTriggerTarget(trigger.Component, descriptor.FloorY)
	if err != nil || observed != input.Target {
		return nil, errors.New("original observed target changed")
	}
	age := now.UnixMilli() - state.SampleUnixMS
	if state.SchemaVersion != executor.PlanarStateSchema || state.RobotID != planar.RobotID || !state.Synthetic ||
		state.RunID != policy.RunID || state.Site != policy.Site || state.Frame != policy.Frame ||
		state.Provenance.Mode != "simulated" || state.Provenance.PackageSHA256 != policy.PackageSHA256 ||
		math.Abs(state.Position[1]-planar.FloorY) > 1e-6 || age < 0 || age > 250 ||
		now.UnixMilli() < policy.NotBeforeUnixMS || now.UnixMilli() >= policy.NotAfterUnixMS {
		return nil, fmt.Errorf("%w: adapter identity, frame, package, floor or freshness differs", errInspectionUnavailable)
	}
	goal, err := collabdemo.NearestReachableGoal(planar.Grid,
		collabdemo.Vec{state.Position[0], -state.Position[2]}, collabdemo.Vec{input.Target[0], -input.Target[2]},
		descriptor.MaxOffsetM, planar.Reservations)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInspectionUnavailable, err)
	}
	target := [3]float64{goal.Goal[0], planar.FloorY, -goal.Goal[1]}
	if missioncontrol.ValidateNativeInspectionTarget(trigger.Component, target, &descriptor) != nil {
		return nil, errors.New("resolved destination differs from original observation")
	}
	route := make([][3]float64, len(goal.Path))
	for i, point := range goal.Path {
		route[i] = [3]float64{point[0], planar.FloorY, -point[1]}
		for axis, value := range route[i] {
			if value < policy.Area.Min[axis] || value > policy.Area.Max[axis] {
				return nil, fmt.Errorf("%w: planned route leaves commissioned area", errInspectionUnavailable)
			}
		}
	}
	travel := int64(math.Ceil(goal.LengthM / policy.SpeedMPS * 1000))
	// The existing executor allows three seconds for its settled four-sensor
	// capture. This nominal budget is review information, not guaranteed arrival.
	minimum := travel + 3000
	if minimum > policy.MaxDurationMS {
		return nil, fmt.Errorf("%w: route exceeds commissioned duration", errInspectionUnavailable)
	}
	return &navigationPlan{Target: target, Descriptor: descriptor, LengthM: goal.LengthM,
		NominalTravelMS: travel, MinimumDurationMS: minimum,
		Review: navigationReview{Version: 1, RobotID: state.RobotID, StartPosition: state.Position,
			StateBootID: state.BootID, StateSession: state.Session, StateSequence: state.Sequence,
			StateSampleUnixMS: state.SampleUnixMS, SimulationTimeS: state.SimulationTimeS, Route: route, OffsetM: goal.OffsetM,
			EstimateBasis: "Nominal route length / commissioned speed + 3000 ms capture allowance; arrival is not guaranteed."}}, nil
}

func prepareNavigation(ctx context.Context, credentials client.Config, input missionInput, directory, tokenPath string) (*navigationPlan, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == "/" || tokenPath == "" {
		return nil, errors.New("explicit commissioned navigation directory and read token required")
	}
	var planar executor.PlanarConfig
	var descriptor missioncontrol.InspectionDestination
	var launch launchConfig
	var signed missioncontrol.SignedContext
	var pins missionclock.Pins
	for name, target := range map[string]any{"planar.json": &planar, "inspection-destination.json": &descriptor,
		"executor-launch.json": &launch, "mission-context.json": &signed, "mission-clock-trust.json": &pins} {
		if err := readJSON(filepath.Join(directory, name), target, 1<<20, false); err != nil {
			return nil, err
		}
	}
	policy := signed.Context
	grant := credentials.Grant.Grant
	if authority.VerifyMissionControlContext(signed, ed25519.PublicKey(pins.RootPublicKey)) != nil ||
		pins.RunID != policy.RunID || pins.ClockDomain != "simulation:"+policy.RunID ||
		grant.Tenant != policy.Tenant || grant.Site != policy.Site || credentials.Audience != policy.Audience ||
		launch.Version != 1 || launch.RunID != policy.RunID || launch.Frame != policy.Frame || !httpOrigin(launch.SceneURL) {
		return nil, errors.New("navigation files differ from commissioned source and proposal scope")
	}
	// Only the read token is loaded. The constructor's unused control slot gets
	// that same read token; this helper calls State exactly once, never Session,
	// Stop or Command. The source does not grant control to this token.
	tokenFile, err := os.Open(tokenPath)
	if err != nil {
		return nil, err
	}
	defer tokenFile.Close()
	info, err := tokenFile.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return nil, errors.New("bounded private source read token required")
	}
	token := make([]byte, info.Size())
	if _, err = io.ReadFull(tokenFile, token); err != nil {
		return nil, err
	}
	readToken := strings.TrimSpace(string(token))
	adapter, err := executor.NewPlanarHTTPAdapter(launch.SceneURL, readToken, readToken, planar.RobotID, planar.FloorY)
	if err != nil {
		return nil, err
	}
	state, err := adapter.State(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: adapter state: %v", errInspectionUnavailable, err)
	}
	return resolveNavigation(input, state, planar, descriptor, policy, time.Now())
}
