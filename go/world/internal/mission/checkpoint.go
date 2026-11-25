package mission

import "errors"

// Checkpoint retains immutable tasks and terminal versions. Deadlines belong to
// a process clock and are deliberately erased from persistent checkpoints.
type Checkpoint struct {
	Records map[string]CheckpointRecord
	Stops   []string
}
type CheckpointRecord struct {
	Task   Task
	Status Status
}

func (e *Engine) Clone() *Engine {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := &Engine{cfg: e.cfg, last: e.last, clockFault: e.clockFault, records: map[string]*record{}, stops: map[string]struct{}{}}
	for id, r := range e.records {
		c := *r
		c.task.Envelope.RequiredSensors = append([]string(nil), r.task.Envelope.RequiredSensors...)
		n.records[id] = &c
	}
	for id := range e.stops {
		n.stops[id] = struct{}{}
	}
	return n
}
func (e *Engine) Checkpoint() Checkpoint {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := Checkpoint{Records: map[string]CheckpointRecord{}}
	for id, r := range e.records {
		t := r.task
		t.Deadline = 0
		t.Envelope.RequiredSensors = append([]string(nil), t.Envelope.RequiredSensors...)
		c.Records[id] = CheckpointRecord{t, r.status}
	}
	for id := range e.stops {
		c.Stops = append(c.Stops, id)
	}
	return c
}

// Restore fails all interrupted work, including observations. No deadline from
// the previous process is interpreted in the new process clock domain.
func Restore(cfg Config, c Checkpoint) (*Engine, error) {
	e, err := New(cfg)
	if err != nil {
		return nil, err
	}
	for id, x := range c.Records {
		r := &record{task: x.Task, digest: Digest(x.Task), status: x.Status}
		if !r.status.Phase.Terminal() {
			e.transition(r, Failed, Stop, "executor restarted; outcome unknown")
		}
		e.records[id] = r
	}
	for _, id := range c.Stops {
		e.stops[id] = struct{}{}
	}
	return e, nil
}

// Fail records a local policy failure without allowing a terminal rewrite.
func (e *Engine) Fail(id, reason string) (Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.records[id]
	if !ok {
		return Status{}, errors.New("unknown task")
	}
	if !r.status.Phase.Terminal() {
		e.transition(r, Failed, Stop, reason)
	}
	return r.status, nil
}
