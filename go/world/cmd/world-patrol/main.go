// world-patrol is an isolated, simulation-only passive mission executor.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusiondemo"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolenvironment"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolkit"
	"github.com/wendylabsinc/wendy/go/world/internal/patroltour"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwork"
	worldstore "github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func atomicJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".patrol-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

type command struct {
	action     string
	reply      chan error
	generation uint64
}
type publication struct {
	sync.RWMutex
	raw []byte
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "kit" {
		return kitCommand(os.Args[2:])
	}
	physics := flag.String("physics", "http://127.0.0.1:8091", "isolated physics HTTP origin")
	nav := flag.String("navigation", "http://127.0.0.1:8092", "isolated Nav2 HTTP origin")
	listen := flag.String("listen", "127.0.0.1:8093", "loopback viewer HTTP address")
	directory := flag.String("out", "", "private fresh run directory")
	schedulePath := flag.String("schedule", "", "reviewed room revisit schedule JSON, immutable for this mission")
	duration := flag.Int("duration", 3600, "mission lifetime in seconds, 1..86400")
	offlineCatalog := flag.String("offline-catalog", "", "trusted local prepared-kit catalog")
	offlineID := flag.String("offline-kit", "", "consume original prepared authority; never sign")
	worldReaderPath := flag.String("world-watch-reader", "", "private commissioned World read credential; never included in signed policy")
	flag.Parse()
	if *directory == "" || *duration < 1 || *duration > 86400 {
		return errors.New("fresh --out and duration 1..86400 required")
	}
	var prepared patrolkit.Verified
	var err error
	if *offlineID != "" || *offlineCatalog != "" {
		prepared, _, err = activation(*offlineCatalog, *offlineID, *directory)
		if err != nil {
			return err
		}
		if *schedulePath != "" {
			return errors.New("offline approval cannot accept a replacement schedule")
		}
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("viewer listener must be loopback")
	}
	if err = os.MkdirAll(*directory, 0700); err != nil {
		return err
	}
	marker, err := os.OpenFile(filepath.Join(*directory, "executor-started"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("executor restart refused; retained mission cannot silently resume")
	}
	if err = marker.Sync(); err != nil {
		marker.Close()
		return err
	}
	if err = marker.Close(); err != nil {
		return err
	}
	markerDir, err := os.Open(*directory)
	if err != nil {
		return err
	}
	err = markerDir.Sync()
	closeErr := markerDir.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, name := range []string{"captures", "evidence"} {
		if err = os.MkdirAll(filepath.Join(*directory, name), 0700); err != nil {
			return err
		}
	}
	a := &adapter{physics: *physics, nav: *nav, dir: *directory, client: &http.Client{Timeout: 12 * time.Second}}
	defer a.Cancel()
	var physical struct {
		ModelSHA256 string         `json:"model_sha256"`
		RunID       string         `json:"run_id"`
		Frame       world.FrameRef `json:"frame"`
	}
	if err = a.request(a.physics, "/state", nil, &physical); err != nil {
		return err
	}
	a.runID = physical.RunID
	a.frame = physical.Frame
	if a.runID == "" || a.frame.Validate() != nil {
		return errors.New("physics scope absent")
	}
	var candidates struct {
		Rooms []patrol.Room `json:"rooms"`
	}
	if err = a.request(a.nav, "/candidates", nil, &candidates); err != nil {
		return err
	}
	if len(candidates.Rooms) == 0 {
		return errors.New("no permitted office rooms")
	}
	var store *tasking.Store
	var epoch uint64
	var auth tasking.Authorization
	var spec tasking.Spec
	var boot *patrolkit.Boot
	var activationSample patrolwork.Sample
	if *offlineID != "" {
		p := prepared.Entry.Profile
		if p.RunID != a.runID || p.Frame != a.frame || p.ModelSHA256 != physical.ModelSHA256 {
			return errors.New("activation physics run, package model or frame differs from signed kit")
		}
		if err = a.request(a.physics, "/work", nil, &activationSample); err != nil {
			return err
		}
		if boot, err = patrolkit.NewBoot(p, activationSample, time.Now().UnixMilli()); err != nil {
			return err
		}
		if err = atomicJSON(filepath.Join(*directory, "activation-boot.json"), boot); err != nil {
			return err
		}
		if candidates.Rooms, err = tourRooms(candidates.Rooms, prepared.Spec.Patrol.Tour); err != nil {
			return err
		}
		if len(prepared.Spec.Patrol.Rooms) != len(candidates.Rooms) {
			return errors.New("activation room inventory differs")
		}
		for i, r := range candidates.Rooms {
			if r.ID != prepared.Spec.Patrol.Rooms[i] {
				return errors.New("activation room order differs")
			}
		}
		store, epoch, auth, spec, err = admitApproval(*directory, a.runID, prepared.Approval, prepared.Entry.RootPublicKey, prepared.Spec)
	} else {
		schedule, e := readPatrolConfig(*schedulePath, candidates.Rooms)
		if e != nil {
			return e
		}
		store, epoch, auth, spec, err = fixture(*directory, a.runID, a.frame, candidates.Rooms, *duration, schedule)
	}
	if err != nil {
		return err
	}
	defer store.Close()
	if spec.Patrol.Exclusions != nil {
		var ack struct {
			Accepted bool   `json:"accepted"`
			Digest   string `json:"policy_digest"`
			Frozen   bool   `json:"policy_frozen"`
		}
		if err = a.request(a.nav, "/policy/commit", map[string]any{"mission_id": spec.ID, "policy": spec.Patrol.Exclusions}, &ack); err != nil {
			return err
		}
		if !ack.Accepted || !ack.Frozen || ack.Digest != spec.Patrol.Exclusions.SHA256 {
			return errors.New("navigation did not freeze the signed restricted areas")
		}
		a.policyDigest = ack.Digest
	}

	if candidates.Rooms, err = tourRooms(candidates.Rooms, spec.Patrol.Tour); err != nil {
		return err
	}
	engine, err := patrol.New(spec.ID, spec.NotAfterUnixMS, candidates.Rooms, a)
	if err != nil {
		return err
	}
	if err = engine.SetSchedule(spec.Patrol.RevisitSeconds, time.Now().UnixMilli()); err != nil {
		return err
	}
	if r := spec.Patrol.Return; r != nil {
		if err = engine.SetReturn(patrol.Point{X: r.Position[0], Y: -r.Position[2], Yaw: r.Yaw}, r.BeforeExpirySeconds); err != nil {
			return err
		}
	}
	if spec.Patrol.Tour != nil {
		if err = a.requireInspectionNavigation(); err != nil {
			return err
		}
		a.inspectionGoal = func() bool { return !engine.State.Return.Requested }
		if err = engine.SetTour(*spec.Patrol.Tour); err != nil {
			return err
		}
	}
	if spec.Patrol.Work != nil {
		var sample patrolwork.Sample
		if boot != nil {
			sample = activationSample
		} else if err = a.request(a.physics, "/work", nil, &sample); err != nil {
			return err
		}
		if err = engine.SetWork(*spec.Patrol.Work, sample, time.Now().UnixMilli(), a.runID, physical.ModelSHA256); err != nil {
			return err
		}
	}
	if spec.Patrol.Environment != nil {
		var source patrolwork.Sample
		if err = a.request(a.physics, "/work", nil, &source); err != nil {
			return err
		}
		if err = engine.SetEnvironment(*spec.Patrol.Environment, a.runID, physical.ModelSHA256, source.BootID, a.frame); err != nil {
			return err
		}
	}
	var watchReader *worldReader
	if spec.Patrol.WorldWatch != nil {
		if err = engine.SetWorldWatch(*spec.Patrol.WorldWatch, a.runID, a.frame); err != nil {
			return err
		}
		watchReader, err = startWorldReader(*worldReaderPath, *spec.Patrol.WorldWatch)
		if err != nil {
			return err
		}
		defer watchReader.close()
	} else if *worldReaderPath != "" {
		return errors.New("World reader requires a signed watch policy")
	}
	observeWorld := func() {
		if watchReader != nil {
			records, transport, until := watchReader.snapshot()
			engine.ObserveWorld(records, transport, until, time.Now())
		}
	}
	observeEnvironment := func() {
		if engine.State.Environment == nil || engine.Terminal() {
			return
		}
		var batch struct {
			Samples []patrolenvironment.Sample `json:"samples"`
		}
		if a.request(a.physics, "/environment", nil, &batch) != nil {
			batch.Samples = nil
		}
		engine.ObserveEnvironment(batch.Samples, time.Now().UnixMilli())
	}
	observeWork := func() {
		if engine.Terminal() || (engine.State.Work == nil && boot == nil) {
			return
		}
		var sample patrolwork.Sample
		var observed *patrolwork.Sample
		if a.request(a.physics, "/work", nil, &sample) == nil {
			observed = &sample
		}
		if boot != nil {
			if e := boot.Observe(observed, time.Now().UnixMilli()); e != nil {
				a.blocked.Store(true)
				if boot.Failed {
					// Preserve the original allowance while marking its last
					// observed source invalid before the terminal state stops
					// ledger updates. This is the same measured reset sample.
					if engine.State.Work != nil {
						engine.ObserveWork(observed, time.Now().UnixMilli())
					}
					engine.State.State = "failed"
					engine.State.Reason = e.Error()
					_ = a.Cancel()
					_ = atomicJSON(filepath.Join(*directory, "activation-boot.json"), boot)
					return
				}
				if engine.State.Work == nil {
					if engine.State.State != "paused" {
						_ = engine.Control("pause", time.Now().UnixMilli())
					}
					engine.State.Reason = e.Error()
				}
			}
		}
		if engine.State.Work != nil {
			engine.ObserveWork(observed, time.Now().UnixMilli())
		}
	}
	a.fuser, err = fusiondemo.New(fusiondemo.Config{Version: 1, ParticipantID: "hub", RunID: a.runID, Frame: a.frame, Sensors: []fusiondemo.Sensor{{SensorID: "camera-a", Kind: "camera", Owner: "hub"}, {SensorID: "camera-b", Kind: "camera", Owner: "hub"}, {SensorID: "gas-1", Kind: "gas", Owner: "hub"}, {SensorID: "audio-1", Kind: "audio", Owner: "hub"}, {SensorID: "lidar-go2", Kind: "lidar", Owner: "go2"}}}, filepath.Join(*directory, "fusion"))
	if err != nil {
		return err
	}
	defer a.fuser.Close()
	a.beforeMotion = func() error {
		observeWork()
		observeEnvironment()
		observeWorld()
		if (boot != nil && !boot.Ready) || !engine.WorkMotionReady() || (!engine.EnvironmentMotionReady() || !engine.WorldMotionReady()) {
			return patrol.ErrInterrupted
		}

		if a.blocked.Load() {
			return patrol.ErrInterrupted
		}
		if time.Now().UnixMilli() >= spec.NotAfterUnixMS {
			return errors.New("mission expired before dispatch")
		}
		var current patrol.Navigation
		if err := a.request(a.nav, "/status", nil, &current); err != nil {
			return err
		}
		if a.inspectionGoal != nil && a.inspectionGoal() && current.InspectionMode != patroltour.NavigationMode {
			return errors.New("navigation fixed-yaw inspection capability unavailable")
		}
		if !current.Ready || current.SensorAge > 1 || (a.policyDigest != "" && (!current.PolicyFrozen || current.PolicyDigest != a.policyDigest)) {
			return errors.New("navigation sensors unavailable before dispatch")
		}
		d, err := store.EvaluatePatrol(spec.ID, executor, epoch, tasking.Telemetry{Frame: a.frame, SampleUnixMS: time.Now().UnixMilli(), Position: [3]float64{current.Pose.X, .3, -current.Pose.Y}, Velocity: [3]float64{current.Speed, 0, 0}, ObstacleClear: true, Energy: 1, Sensors: map[string]bool{"lidar": true, "odometry": true, "localization": true}})
		if err != nil {
			return err
		}
		if d.Action != mission.Continue {
			return errors.New("mission authority no longer permits motion")
		}
		observeWork()
		observeEnvironment()
		observeWorld()
		if (boot != nil && !boot.Ready) || !engine.WorkMotionReady() || (!engine.EnvironmentMotionReady() || !engine.WorldMotionReady()) {
			return patrol.ErrInterrupted
		}
		if a.blocked.Load() || time.Now().UnixMilli() >= spec.NotAfterUnixMS {
			return errors.New("motion authorization ended")
		}
		return nil
	}
	a.beforeCapture = func() error {
		observeWork()
		observeEnvironment()
		observeWorld()
		if (boot != nil && !boot.Ready) || !engine.WorkMotionReady() || (!engine.EnvironmentMotionReady() || !engine.WorldMotionReady()) {
			return patrol.ErrInterrupted
		}

		if a.blocked.Load() {
			return patrol.ErrInterrupted
		}
		if time.Now().UnixMilli() >= spec.NotAfterUnixMS {
			return errors.New("observation expired")
		}
		var current patrol.Navigation
		if err := a.request(a.nav, "/status", nil, &current); err != nil {
			return err
		}
		p := engine.State.Destination
		if spec.Patrol.Tour != nil && (p == nil || math.IsNaN(current.Pose.X) || math.IsInf(current.Pose.X, 0) || math.IsNaN(current.Pose.Y) || math.IsInf(current.Pose.Y, 0) || math.IsNaN(current.Pose.Yaw) || math.IsInf(current.Pose.Yaw, 0) || math.Abs(math.Atan2(math.Sin(current.Pose.Yaw-p.Yaw), math.Cos(current.Pose.Yaw-p.Yaw))) > .15) {
			return errors.New("signed tour observation pose changed")
		}
		if !current.Ready || current.SensorAge > 1 || p == nil || math.Hypot(current.Pose.X-p.X, current.Pose.Y-p.Y) > .4 || math.Abs(current.Speed) > .12 || math.Abs(current.AngularSpeed) > .15 {
			return errors.New("capture pose or freshness changed")
		}
		return nil
	}
	replica, err := worldstore.Open(filepath.Join(*directory, "progress-world.json"), world.Config{Tenant: "simulation", Site: "studio-01", ClockDomain: "simulation-executor-wall:" + a.runID, Now: time.Now, MaxEntries: 2, AllowSynthetic: true, Frames: []world.FrameRef{a.frame}})
	if err != nil {
		return err
	}
	defer replica.Close()
	var componentSequence uint64
	var componentTime time.Time
	var components []world.Component
	tokenBytes := make([]byte, 32)
	if _, err = rand.Read(tokenBytes); err != nil {
		return err
	}
	token := hex.EncodeToString(tokenBytes)
	if err = os.WriteFile(filepath.Join(*directory, "control-token"), []byte(token), 0600); err != nil {
		return err
	}
	commands := make(chan command, 8)
	fence := &controlFence{blocked: &a.blocked}
	var published publication
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		published.RLock()
		defer published.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(published.raw)
	})
	mux.HandleFunc("POST /control", controlHandler(token, commands, fence, func() { _ = a.Cancel() }))
	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer server.Close()
	initialRecord, _ := store.Get(spec.ID)
	published.raw, _ = json.Marshal(map[string]any{"patrol": engine.State, "navigation": patrol.Navigation{}, "task": initialRecord, "run_id": a.runID})
	go server.Serve(listener)
	if err = atomicJSON(filepath.Join(*directory, "executor-ready.json"), map[string]any{"url": "http://" + listener.Addr().String(), "run_id": a.runID, "task_id": spec.ID, "expires_at_unix_ms": spec.NotAfterUnixMS}); err != nil {
		return err
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	var lastPersist string
	var navigation patrol.Navigation
	for {
		select {
		case <-signals:
			a.blocked.Store(true)
			_ = engine.Control("stop", time.Now().UnixMilli())
			_, _ = store.Stop(spec.ID, auth)
			_ = atomicJSON(filepath.Join(*directory, "patrol.json"), engine.State)
			return nil
		case c := <-commands:
			if c.action == "resume" || c.action == "recall" {
				observeWork()
				observeEnvironment()
				observeWorld()
			}
			var e error
			if (c.action == "resume" || c.action == "recall") && boot != nil && !boot.Ready {
				e = errors.New(boot.Reason)
			} else {
				e = applyControl(c, fence, engine, time.Now().UnixMilli())
			}
			if c.action == "stop" && e == nil {
				_, e = store.Stop(spec.ID, auth)
			}
			c.reply <- e
		case <-tick.C:
			observeWork()
			observeEnvironment()
			observeWorld()
			navigation = patrol.Navigation{}
			err = a.request(a.nav, "/status", nil, &navigation)
			if err != nil || (a.policyDigest != "" && (!navigation.PolicyFrozen || navigation.PolicyDigest != a.policyDigest)) {
				navigation.Ready = false
			}
			// The admitted envelope sees AMCL pose only. Physics truth is never requested here.
			sensors := map[string]bool{"lidar": navigation.Ready, "odometry": navigation.Ready, "localization": navigation.Ready}
			decision, e := store.EvaluatePatrol(spec.ID, executor, epoch, tasking.Telemetry{Frame: a.frame, SampleUnixMS: time.Now().UnixMilli(), Position: [3]float64{navigation.Pose.X, .3, -navigation.Pose.Y}, Velocity: [3]float64{navigation.Speed, 0, 0}, ObstacleClear: navigation.Ready, Energy: 1, Sensors: sensors})
			if decision.Record.Status.Phase.Terminal() {
				engine.State.State = "failed"
				engine.State.Reason = decision.Record.Status.Reason
				_ = a.Cancel()
			}
			if time.Now().UnixMilli() >= spec.NotAfterUnixMS {
				engine.State.State = "expired"
				_ = a.Cancel()
			}
			if e != nil || decision.Action != mission.Continue {
				navigation.Ready = false
			}
			if navigation.Ready && engine.State.Destination == nil && (engine.State.State == "waiting" || engine.State.State == "blocked" || engine.State.State == "scheduled") {
				var fresh struct {
					Rooms []patrol.Room `json:"rooms"`
				}
				if err = a.request(a.nav, "/candidates", nil, &fresh); err != nil {
					navigation.Ready = false
				} else {
					for i := range engine.State.Rooms {
						engine.State.Rooms[i].Candidates = nil
						for _, room := range fresh.Rooms {
							if room.ID == engine.State.Rooms[i].ID {
								engine.State.Rooms[i].Candidates = room.Candidates
								engine.State.Rooms[i].CandidateClearance = room.CandidateClearance
								engine.State.Rooms[i].CandidateBlockedReason = room.CandidateBlockedReason
							}
						}
					}
				}
			}
			if e = engine.Tick(time.Now().UnixMilli(), navigation); e != nil {
				engine.State.State = "failed"
				engine.State.Reason = e.Error()
			}
			if (engine.State.State == "navigating" || engine.State.State == "returning") && navigation.Ready && !a.blocked.Load() {
				e = a.beforeMotion()
				if e == nil {
					e = a.request(a.nav, "/heartbeat", map[string]any{"expires_at_unix_ms": spec.NotAfterUnixMS}, nil)
				}
				if e != nil {
					if engine.State.State != "work_held" && engine.State.State != "environment_held" && engine.State.State != "world_held" && !(boot != nil && !boot.Ready) {
						engine.State.State = "waiting"
						if engine.State.Return != nil && engine.State.Return.Requested {
							engine.State.State = "return_held"
						}
						engine.State.Reason = e.Error()
					}
					_ = a.Cancel()
				}
			}
			observeWork()
			observeEnvironment()
			observeWorld()
			if since, first, last, arrived := engine.Arrival(); arrived && engine.WorkMotionReady() && engine.EnvironmentMotionReady() && engine.WorldMotionReady() && !(boot != nil && !boot.Ready) {
				observed := time.Now().UnixMilli()
				arrival := &tasking.PatrolArrival{MissionID: spec.ID, SettledSinceUnixMS: since, ObservedAtUnixMS: observed, FirstSimulationNS: int64(first * 1e9), LastSimulationNS: int64(last * 1e9), AngularSpeed: navigation.AngularSpeed, Tour: engine.TourEvidence()}
				if !engine.TourComplete() {
					engine.State.State = "failed"
					engine.State.Reason = "Inspection tour incomplete: returned without every required original capture"
					engine.State.Tour.Outcome = "incomplete"
				} else {
					result, err := store.EvaluatePatrol(spec.ID, executor, epoch, tasking.Telemetry{Frame: a.frame, SampleUnixMS: observed, SampleSimulationNS: int64(navigation.Stamp * 1e9), PatrolArrival: arrival, Position: [3]float64{navigation.Pose.X, .3, -navigation.Pose.Y}, Velocity: [3]float64{navigation.Speed, 0, 0}, ObstacleClear: navigation.Ready, Energy: 1, Sensors: sensors})
					if err == nil && result.Record.Status.Phase == mission.Succeeded {
						engine.State.State = "completed"
						if engine.State.Tour != nil {
							engine.State.Tour.Outcome = "completed"
						}
						engine.State.Return.ArrivedAtMS = observed
						engine.State.Reason = "Returned to the approved rendezvous and settled; mission complete"
					} else if result.Record.Status.Phase.Terminal() {
						engine.State.State = "failed"
						engine.State.Reason = result.Record.Status.Reason
					} else {
						engine.State.State = "return_held"
						engine.State.Reason = "Return arrival was not accepted; resume after checking current conditions"
					}
				}
				_ = a.Cancel()
			}
		}
		record, _ := store.Get(spec.ID)
		if engine.Terminal() && !record.Status.Phase.Terminal() {
			if engine.State.State == "stopped" {
				record, err = store.Stop(spec.ID, auth)
			} else {
				record, err = store.Report(spec.ID, executor, epoch, record.Status.Version+1, mission.Failed)
			}
			if err != nil {
				engine.State.Reason += "; task status: " + err.Error()
			}
		}
		if engine.Terminal() && engine.State.Tour != nil && engine.State.State != "completed" {
			engine.State.Tour.Outcome = "incomplete"
		}
		// Persist coverage and task status together for inspection; store owns task authority.
		data, _ := json.Marshal(engine.State)
		if string(data) != lastPersist {
			if err = atomicJSON(filepath.Join(*directory, "patrol.json"), engine.State); err != nil {
				return err
			}
			lastPersist = string(data)
		}
		if time.Since(componentTime) >= time.Second || engine.Terminal() {
			componentSequence++
			componentTime = time.Now()
			components, err = patrol.Components(engine.State, a.runID, a.frame, componentSequence, componentTime)
			if err != nil {
				return err
			}
			for _, c := range components {
				if _, err = replica.Apply(c.Key.Source, c); err != nil {
					return err
				}
			}
		}
		state := map[string]any{"patrol": engine.State, "navigation": navigation, "task": record, "fusion": a.latest, "run_id": a.runID, "components": components, "restricted_areas": spec.Patrol.Exclusions, "offline_kit": spec.Patrol.Offline, "activation_boot": boot}
		raw, e := json.Marshal(state)
		if e != nil {
			return e
		}
		published.Lock()
		published.raw = raw
		published.Unlock()
		if engine.Terminal() {
			if !record.Status.Phase.Terminal() {
				if engine.State.State == "stopped" {
					_, _ = store.Stop(spec.ID, auth)
				} else {
					_, _ = store.Report(spec.ID, executor, epoch, record.Status.Version+1, mission.Failed)
				}
			}
			if err = atomicJSON(filepath.Join(*directory, "final-status.json"), state); err != nil {
				return err
			}
			// Keep the read endpoint and retained evidence available after mission end.
			fmt.Printf("patrol %s: %s\n", engine.State.State, engine.State.Reason)
			for {
				select {
				case <-signals:
					return nil
				case c := <-commands:
					if c.action == "resume" || c.action == "recall" {
						observeWork()
						observeEnvironment()
						observeWorld()
					}
					c.reply <- errors.New("mission has ended")
				}
			}
		}
	}
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error()))
		os.Exit(1)
	}
}
