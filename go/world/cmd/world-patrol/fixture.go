package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolkit"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const audience = "go2-patrol-simulation"
const executor = "patrol/executor"

func fixture(dir, runID string, frame world.FrameRef, rooms []patrol.Room, duration int, schedule ...scheduleDocument) (*tasking.Store, uint64, tasking.Authorization, tasking.Spec, error) {
	approval, spec, err := signFixture(runID, frame, rooms, duration, schedule...)
	if err != nil {
		return nil, 0, tasking.Authorization{}, spec, err
	}
	return admitApproval(dir, runID, approval, approval.RootPublicKey, spec)
}
func signFixture(runID string, frame world.FrameRef, rooms []patrol.Room, duration int, schedule ...scheduleDocument) (patrolkit.Approval, tasking.Spec, error) {
	var zero tasking.Spec
	now := time.Now()
	until := now.Add(time.Duration(duration) * time.Second)
	pub, root, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return patrolkit.Approval{}, zero, err
	}
	op, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return patrolkit.Approval{}, zero, err
	}
	grant, err := authority.SignGrant(authority.Grant{Version: 1, ID: runID + "/operator", Tenant: "simulation", Site: "studio-01", Domain: "simulation", Principal: "patrol/fixture-operator", Kind: "operator", Source: world.Source{Device: "fixture", App: "wendy-world", Service: "patrol", Stream: "missions"}, PublicKey: op, Scopes: []string{"mission", "stop"}, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: until.Add(time.Second).UnixMilli()}, root)
	if err != nil {
		return patrolkit.Approval{}, zero, err
	}
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "simulation", Site: "studio-01", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: until.Add(time.Second).UnixMilli()}, root)
	if err != nil {
		return patrolkit.Approval{}, zero, err
	}
	if len(schedule) > 0 && schedule[0].Tour != nil {
		rooms, err = tourRooms(rooms, schedule[0].Tour)
		if err != nil {
			return patrolkit.Approval{}, zero, err
		}
	}
	ids := []string{}
	for _, r := range rooms {
		ids = append(ids, r.ID)
	}
	spec := tasking.Spec{ID: runID + "/patrol", Type: tasking.PatrolType, Site: "studio-01", Assignee: "go2/navigation", Domain: mission.Simulation, Class: mission.Move, Mode: mission.Autonomous, Frame: frame, NotAfterUnixMS: until.UnixMilli(), Area: tasking.Bounds{Min: [3]float64{-9, -1, -7}, Max: [3]float64{9, 2, 7}}, RequiredSensors: []string{"lidar", "odometry", "localization"}, Fallback: mission.Hold, Target: [3]float64{-6, .3, 0}, SpeedMPS: .55, ToleranceM: .4, StopSpeedMPS: .12, Patrol: &tasking.PatrolPolicy{Rooms: ids, DurationSeconds: duration, ObservationPolicy: "observe-and-report", DwellSeconds: 2, NoProgressSeconds: 15, DeferSeconds: 30}}
	if len(schedule) > 0 {
		spec.Patrol.Tour = schedule[0].Tour
		spec.Patrol.RevisitSeconds = schedule[0].RevisitSeconds
		spec.Patrol.Return = schedule[0].Return
		spec.Patrol.Exclusions = schedule[0].Exclusions
		spec.Patrol.Work = schedule[0].Work
		spec.Patrol.Environment = schedule[0].Environment
		spec.Patrol.WorldWatch = schedule[0].WorldWatch
		spec.Patrol.Offline = schedule[0].Offline
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return patrolkit.Approval{}, zero, err
	}
	instruction, err := authority.SignInstruction(grant, key, audience, "mission", raw, now, time.Duration(duration)*time.Second)
	if err != nil {
		return patrolkit.Approval{}, zero, err
	}
	return patrolkit.Approval{Instruction: instruction, RootPublicKey: pub, Revocations: rev, Fixture: true, Hardware: false}, spec, nil
}
func admitApproval(dir, runID string, approval patrolkit.Approval, pin []byte, spec tasking.Spec) (*tasking.Store, uint64, tasking.Authorization, tasking.Spec, error) {
	now := time.Now()
	replay, err := authority.OpenReplay(filepath.Join(dir, "authority.json"), 128)
	if err != nil {
		return nil, 0, tasking.Authorization{}, spec, err
	}
	verifier, err := authority.NewVerifier(authority.Config{RootPublicKey: pin, Tenant: "simulation", Site: "studio-01", Domain: "simulation", Audience: audience, Now: time.Now, Replay: replay, MaxRevocationAge: 25 * time.Hour, MaxGrantLifetime: 25 * time.Hour})
	if err == nil {
		err = verifier.UpdateRevocations(approval.Revocations)
	}
	if err != nil {
		return nil, 0, tasking.Authorization{}, spec, err
	}
	instruction := approval.Instruction

	identity, err := verifier.VerifyInstruction(instruction, "mission")
	if err != nil {
		return nil, 0, tasking.Authorization{}, spec, err
	}
	proof, _ := json.Marshal(instruction)
	auth := tasking.Authorization{Principal: identity.Principal, Operator: true, AllowAutonomy: true, ValidUntil: identity.NotAfter, Proof: proof}
	store, err := tasking.Open(filepath.Join(dir, "tasks.json"), tasking.Config{RunID: runID, Site: spec.Site, Assignee: spec.Assignee, ExecutorPrincipal: executor, Domain: mission.Simulation, Catalog: map[string]mission.SafetyClass{tasking.PatrolType: mission.Move}, Now: time.Now, Monotonic: func() time.Duration { return time.Since(now) }, MaxLease: time.Second, MaxTelemetryAge: time.Second, ValidateAuthority: func(r tasking.Record) error {
		var i authority.SignedInstruction
		if err := json.Unmarshal(r.Proof, &i); err != nil {
			return err
		}
		bound, _ := json.Marshal(r.Spec)
		if !bytes.Equal(bound, i.Payload) {
			return errors.New("mission differs from signed instruction")
		}
		_, err := verifier.VerifyInstruction(i, "mission")
		return err
	}})
	if err != nil {
		return nil, 0, auth, spec, err
	}
	epoch, err := store.BeginExecutor(executor)
	if err == nil {
		_, err = store.Accept(spec, auth)
	}
	if err == nil {
		err = atomicJSON(filepath.Join(dir, "mission.json"), approval)
	}
	return store, epoch, auth, spec, err
}
