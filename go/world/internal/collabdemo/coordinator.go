package collabdemo

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

type worker struct {
	job            int
	path, reserved []Vec
	position       Vec // Last position received by the coordinator, frozen while offline.
	online         bool
	inspecting     bool
	inspectSince   float64
}
type Coordinator struct {
	exclusions []Exclusion
	scene      Scene
	blocked    map[Cell]bool
	workers    map[string]*worker
	jobs       []JobOutput
	runID      string
	sequence   uint64
	time       float64
	seen       bool
	started    bool
	deadline   float64
	terminal   string
}

func New(scene Scene) (*Coordinator, error) {
	g := scene.Grid
	if !validVec(g.Origin) || !finite(g.Resolution) || g.Resolution <= 0 || g.Resolution > 2 || g.Width < 1 || g.Height < 1 || g.Width > 512 || g.Height > 512 || g.Width*g.Height > 100000 || len(scene.Robots) < 1 || len(scene.Robots) > 16 || len(scene.Jobs) < 1 || len(scene.Jobs) > 128 {
		return nil, errors.New("invalid bounded scene")
	}
	c := &Coordinator{scene: scene, blocked: map[Cell]bool{}, workers: map[string]*worker{}}
	for _, b := range g.Blocked {
		if !g.inside(b) {
			return nil, errors.New("blocked cell outside grid")
		}
		c.blocked[b] = true
	}
	for _, r := range scene.Robots {
		if r.ID == "" || len(r.ID) > 128 || !validVec(r.Start) || !g.inside(g.cell(r.Start)) || c.blocked[g.cell(r.Start)] || c.workers[r.ID] != nil {
			return nil, errors.New("invalid or duplicated robot")
		}
		c.workers[r.ID] = &worker{job: -1, position: r.Start}
	}
	ids := map[string]bool{}
	for _, j := range scene.Jobs {
		if j.ID == "" || len(j.ID) > 128 || ids[j.ID] || !validVec(j.Position) || !g.inside(g.cell(j.Position)) || c.blocked[g.cell(j.Position)] {
			return nil, errors.New("invalid or duplicated job")
		}
		ids[j.ID] = true
		c.jobs = append(c.jobs, JobOutput{ID: j.ID, Label: j.Label, State: "pending"})
	}
	return c, nil
}

func (c *Coordinator) validate(in Input) (map[string]Observation, error) {
	if in.RunID == "" || len(in.RunID) > 256 || !finite(in.Time) || in.Time < 0 || len(in.Robots) != len(c.workers) {
		return nil, errors.New("invalid run, time or robot set")
	}
	if c.seen && (in.RunID != c.runID || in.Sequence <= c.sequence || in.Time <= c.time) {
		return nil, errors.New("run changed or sequence/time did not advance")
	}
	obs := map[string]Observation{}
	for _, r := range in.Robots {
		if c.workers[r.ID] == nil || !validVec(r.Position) || !validVec(r.Velocity) || !c.scene.Grid.inside(c.scene.Grid.cell(r.Position)) {
			return nil, errors.New("invalid robot telemetry")
		}
		if _, exists := obs[r.ID]; exists {
			return nil, errors.New("duplicate robot telemetry")
		}
		obs[r.ID] = r
	}
	if c.started && !sameExclusions(in.Exclusions, c.exclusions) {
		return nil, errors.New("route exclusions are immutable after mission start")
	}
	if in.Preview != nil {
		if c.workers[in.Preview.RobotID] == nil {
			return nil, errors.New("unknown preview robot")
		}
		found := false
		for _, job := range c.scene.Jobs {
			if job.ID == in.Preview.JobID {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("unknown preview destination")
		}
	}
	return obs, nil
}

func (c *Coordinator) output(in Input, phase string) Output {
	o := Output{Exclusions: append([]Exclusion(nil), c.exclusions...), ExclusionsFrozen: c.started, RunID: in.RunID, Sequence: in.Sequence, Time: in.Time, Phase: phase, Jobs: append([]JobOutput(nil), c.jobs...), Total: len(c.jobs), Events: []string{}}
	for _, j := range c.jobs {
		if j.State == "completed" {
			o.Completed++
		}
	}
	for _, r := range c.scene.Robots {
		w := c.workers[r.ID]
		v := RobotOutput{ID: r.ID, State: phase, Path: []Vec{}}
		if w.job >= 0 {
			v.JobID = c.scene.Jobs[w.job].ID
			v.Path = append(v.Path, w.path...)
		}
		o.Robots = append(o.Robots, v)
	}
	return o
}

// Refused emits no motion and changes no coordinator state.
func (c *Coordinator) Refused(in Input, reason string) Output {
	if !finite(in.Time) {
		in.Time = 0
	}
	o := c.output(in, "refused")
	o.Events = append(o.Events, reason)
	return o
}

func (c *Coordinator) Tick(in Input) Output {
	obs, err := c.validate(in)
	if err != nil {
		return c.Refused(in, err.Error())
	}
	if !c.started {
		blocked, err := c.exclusionGrid(in.Exclusions, obs)
		if err != nil {
			return c.Refused(in, err.Error())
		}
		c.blocked = blocked
		c.exclusions = append([]Exclusion(nil), in.Exclusions...)
	}
	c.seen = true
	c.runID = in.RunID
	c.sequence = in.Sequence
	c.time = in.Time
	events := []string{}
	for _, r := range c.scene.Robots {
		w, o := c.workers[r.ID], obs[r.ID]
		if w.online != o.Online {
			if o.Online {
				events = append(events, r.ID+" link restored")
				// The local worker can advance along its cached route while its
				// application link is isolated. Reconcile only fresh telemetry.
				if len(w.path) > 1 {
					nearest, best := 1, math.Inf(1)
					for i := 1; i < len(w.path); i++ {
						d := segmentDistance(o.Position, w.path[i-1], w.path[i])
						if d < best {
							best, nearest = d, i
						}
					}
					if best <= .20 {
						w.path = w.path[nearest:]
					}
				}
			} else {
				events = append(events, r.ID+" link disconnected; cached work and reservation retained")
			}
		}
		w.online = o.Online
		if o.Online {
			w.position = o.Position
		}
	}
	if in.Stop && c.terminal == "" {
		c.terminal = "stopped"
		events = append(events, "Operator stop latched; a new process and explicit authorization are required")
	}
	if c.started && in.Time >= c.deadline && c.terminal == "" {
		c.terminal = "expired"
		events = append(events, "Simulation mission deadline expired")
	}
	if c.terminal != "" {
		for i := range c.jobs {
			if c.jobs[i].State != "completed" {
				c.jobs[i].State = c.terminal
			}
		}
		o := c.output(in, c.terminal)
		o.Events = events
		return o
	}
	if !in.Authorized {
		for _, w := range c.workers {
			w.inspecting = false
		}
		o := c.output(in, "awaiting_authorization")
		o.Preview = c.preview(in.Preview)
		o.Events = events
		return o
	}
	if !c.started {
		c.started = true
		c.deadline = in.Time + 600
		events = append(events, "Explicit simulation authorization accepted; 600 s deadline")
	}
	// Release stopped work before allocating any new job. Disconnected
	// workers retain reservations and ownership, never implicit reassignment.
	for _, r := range c.scene.Robots {
		w, o := c.workers[r.ID], obs[r.ID]
		if w.job < 0 {
			continue
		}
		if !o.Available {
			w.inspecting = false
			if o.Online && math.Hypot(o.Velocity[0], o.Velocity[1]) < .02 {
				j := &c.jobs[w.job]
				j.State = "pending"
				j.AssignedTo = ""
				events = append(events, r.ID+" observed stopped; unfinished job released")
				w.job = -1
				w.path = nil
				w.reserved = nil
			}
		}
	}
	for _, r := range c.scene.Robots {
		w, o := c.workers[r.ID], obs[r.ID]
		if w.job >= 0 || !o.Online || !o.Available {
			continue
		}
		indices := []int{}
		for i, j := range c.jobs {
			if j.State == "pending" {
				indices = append(indices, i)
			}
		}
		sort.SliceStable(indices, func(i, j int) bool {
			return distance(o.Position, c.scene.Jobs[indices[i]].Position) < distance(o.Position, c.scene.Jobs[indices[j]].Position)
		})
		reservations := c.reservations(r.ID)
		for _, index := range indices {
			path := c.plan(o.Position, c.scene.Jobs[index].Position, reservations)
			if len(path) == 0 {
				continue
			}
			w.job = index
			w.path = path
			w.reserved = append([]Vec(nil), path...)
			w.inspecting = false
			c.jobs[index].AssignedTo = r.ID
			c.jobs[index].State = "assigned"
			events = append(events, fmt.Sprintf("%s reserved route to %s", r.ID, c.jobs[index].Label))
			break
		}
	}
	o := c.output(in, "running")
	o.Events = events
	for i, r := range c.scene.Robots {
		w, measured := c.workers[r.ID], obs[r.ID]
		v := &o.Robots[i]
		if !measured.Online {
			w.inspecting = false
			v.State = "offline_wait"
			if w.job >= 0 {
				v.State = "offline_cached"
			}
			continue
		}
		if !measured.Available {
			v.State = "paused"
			if math.Hypot(measured.Velocity[0], measured.Velocity[1]) >= .02 {
				v.State = "stopping"
			}
			continue
		}
		if w.job < 0 {
			v.State = "waiting"
			if !measured.Online {
				v.State = "offline_wait"
			}
			continue
		}
		goal := c.scene.Jobs[w.job].Position
		if distance(measured.Position, goal) < .08 {
			v.State = "settling"
			if math.Hypot(measured.Velocity[0], measured.Velocity[1]) >= .03 {
				w.inspecting = false
				continue
			}
			v.State = "inspecting"
			if !w.inspecting {
				w.inspectSince = in.Time
				w.inspecting = true
			}
			if in.Time-w.inspectSince >= 2 {
				j := &c.jobs[w.job]
				j.State = "completed"
				j.Reading = &Reading{GasPPM: 1 + 80*math.Exp(-(measured.Position[0]*measured.Position[0]+measured.Position[1]*measured.Position[1])/12.5), Position: measured.Position, Time: in.Time, Provenance: "synthetic-analytic"}
				v.State = "completed"
				o.Events = append(o.Events, r.ID+" completed "+j.Label)
				w.job = -1
				w.path = nil
				w.reserved = nil
				w.inspecting = false
			}
			continue
		}
		w.inspecting = false
		for len(w.path) > 1 && distance(measured.Position, w.path[0]) < .07 {
			w.path = w.path[1:]
		}
		if measured.Online {
			w.reserved = append([]Vec{measured.Position}, w.path...)
		}
		v.Path = append([]Vec{}, w.path...)
		if len(w.path) == 0 {
			v.State = "waiting"
			continue
		}
		// Runtime telemetry is also the modeled local collision observation.
		// It can veto motion but never assign work or release an offline reservation.
		target := w.path[0]
		d := distance(measured.Position, target)
		if d < 1e-9 {
			v.State = "waiting"
			continue
		}
		speed := math.Min(MaxSpeed, d*3)
		velocity := Vec{(target[0] - measured.Position[0]) / d * speed, (target[1] - measured.Position[1]) / d * speed}
		clear := true
		predicted := Vec{measured.Position[0] + velocity[0]*.35, measured.Position[1] + velocity[1]*.35}
		for id, peer := range obs {
			if id != r.ID && segmentDistance(peer.Position, measured.Position, predicted) < Separation {
				clear = false
				break
			}
		}
		if !clear {
			v.State = "yielding"
			continue
		}
		v.Velocity = velocity
		v.State = "moving"
		if !measured.Online {
			v.State = "moving_offline"
		}
	}
	o.Jobs = append([]JobOutput(nil), c.jobs...)
	o.Completed = 0
	for _, j := range c.jobs {
		if j.State == "completed" {
			o.Completed++
		}
	}
	if o.Completed == o.Total {
		o.Phase = "completed"
		for i := range o.Robots {
			o.Robots[i].Velocity = Vec{}
		}
	}
	return o
}

func (c *Coordinator) reservations(exclude string) [][]Vec {
	var result [][]Vec
	for _, r := range c.scene.Robots {
		if r.ID == exclude {
			continue
		}
		w := c.workers[r.ID]
		result = append(result, []Vec{w.position})
		if len(w.reserved) > 0 {
			result = append(result, w.reserved)
		}
	}
	return result
}
