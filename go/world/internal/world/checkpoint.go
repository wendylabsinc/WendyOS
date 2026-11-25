package world

import (
	"encoding/hex"
	"errors"
	"sort"
	"time"
)

// Checkpoint is trusted local persistence state, not a network admission format.
// It retains all high-water entries, including tombstones and expired records.
type Checkpoint struct {
	Fences      []CheckpointFence `json:"fences,omitempty"`
	Version     int               `json:"version"`
	Tenant      string            `json:"tenant"`
	Site        string            `json:"site"`
	ClockDomain string            `json:"clock_domain"`
	CapturedAt  time.Time         `json:"captured_at"`
	Frames      []FrameRef        `json:"frames"`
	Entries     []CheckpointEntry `json:"entries"`
}

// CheckpointFence is trusted local replay suppression, never an origin proof.
type CheckpointFence struct {
	Key     Key     `json:"key"`
	Version Version `json:"version"`
	SHA256  string  `json:"sha256"`
}

type CheckpointEntry struct {
	Component Component `json:"component"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Checkpoint advances the observed clock and copies every retained entry.
func (r *Replica) Checkpoint() (Checkpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now, err := r.now()
	if err != nil {
		return Checkpoint{}, err
	}
	s := Checkpoint{Version: 1, Tenant: r.config.Tenant, Site: r.config.Site, ClockDomain: r.config.ClockDomain, CapturedAt: now.Round(0), Frames: []FrameRef{}, Entries: []CheckpointEntry{}}
	for id, revision := range r.frames {
		s.Frames = append(s.Frames, FrameRef{ID: id, Revision: revision})
	}
	sort.Slice(s.Frames, func(i, j int) bool { return s.Frames[i].ID < s.Frames[j].ID })
	for _, e := range r.entries {
		if e.fenced {
			s.Fences = append(s.Fences, CheckpointFence{e.component.Key, e.component.Version, e.identity})
			s.Version = 2
			continue
		}
		s.Entries = append(s.Entries, CheckpointEntry{Component: cloneComponent(e.component), ExpiresAt: e.expires.Round(0)})
	}
	sort.Slice(s.Entries, func(i, j int) bool {
		return keyString(s.Entries[i].Component.Key) < keyString(s.Entries[j].Component.Key)
	})
	sort.Slice(s.Fences, func(i, j int) bool { return keyString(s.Fences[i].Key) < keyString(s.Fences[j].Key) })
	return s, nil
}

// Restore validates local persistence state before reconstructing a replica.
// The caller must supply a clock that never precedes the durable checkpoint.
func Restore(config Config, state Checkpoint) (*Replica, error) {
	if (state.Version != 1 && state.Version != 2) || (state.Version == 1 && len(state.Fences) != 0) || state.Tenant != config.Tenant || state.Site != config.Site || state.ClockDomain != config.ClockDomain || state.CapturedAt.IsZero() {
		return nil, errors.New("checkpoint format or scope mismatch")
	}
	if config.Now == nil {
		return nil, errors.New("missing clock")
	}
	now := config.Now()
	if now.IsZero() || now.Before(state.CapturedAt) {
		return nil, ErrClockRegression
	}
	// Admission at the checkpoint instant validates original TTL limits without
	// allowing an already expired record to acquire a new lifetime on restore.
	validation := config
	validation.Now = func() time.Time { return state.CapturedAt }
	validation.Frames = state.Frames
	r, err := NewReplica(validation)
	if err != nil {
		return nil, err
	}
	for _, stored := range state.Entries {
		c := stored.Component
		if _, exists := r.entries[c.Key]; exists {
			return nil, errors.New("duplicate checkpoint key")
		}
		if _, err := r.Apply(c.Key.Source, c); err != nil {
			return nil, err
		}
		e := r.entries[c.Key]
		if stored.ExpiresAt.IsZero() || stored.ExpiresAt.Round(0).After(e.expires.Round(0)) {
			return nil, errors.New("checkpoint extends record expiry")
		}
		// Bind the durable absolute deadline to this process's monotonic clock.
		// Subsequent transaction clones preserve it without recomputing age.
		e.expires = now.Add(stored.ExpiresAt.Round(0).Sub(now.Round(0)))
		r.entries[c.Key] = e
	}
	if len(state.Entries)+len(state.Fences) > config.MaxEntries {
		return nil, ErrCapacity
	}
	for _, f := range state.Fences {
		h, e := hex.DecodeString(f.SHA256)
		if e != nil || len(h) != 32 || hex.EncodeToString(h) != f.SHA256 || f.Key.Tenant != config.Tenant || f.Key.Site != config.Site || f.Key.Entity == "" || f.Key.Type == "" || f.Key.Source.Validate() != nil || f.Version.Epoch == 0 || f.Version.Sequence == 0 {
			return nil, errors.New("invalid checkpoint fence")
		}
		if _, exists := r.entries[f.Key]; exists {
			return nil, errors.New("duplicate checkpoint fence")
		}
		r.entries[f.Key] = entry{component: Component{Key: f.Key, Version: f.Version}, identity: f.SHA256, fenced: true}
	}

	r.config = config
	r.config.Frames = nil
	r.lastNow = now
	return r, nil
}

// Clone makes a private transaction candidate while preserving this process's
// monotonic deadline readings. Persistence serialization must use Checkpoint;
// restoring serialized wall timestamps is only necessary after process loss.
func (r *Replica) Clone() (*Replica, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.now(); err != nil {
		return nil, err
	}
	out := &Replica{config: r.config, frames: make(map[string]string, len(r.frames)), entries: make(map[Key]entry, len(r.entries)), lastNow: r.lastNow}
	for k, v := range r.frames {
		out.frames[k] = v
	}
	for k, v := range r.entries {
		v.component = cloneComponent(v.component)
		out.entries[k] = v
	}
	return out, nil
}

// Records returns every retained source register, including tombstones and
// expired versions. The raised age hint preserves locally shortened expiry
// when a peer receives a record through synchronization.
func (r *Replica) Records() ([]Component, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now, err := r.now()
	if err != nil {
		return nil, err
	}
	result := make([]Component, 0, len(r.entries))
	for _, e := range r.entries {
		if e.fenced {
			continue
		}
		c := cloneComponent(e.component)
		remaining := e.expires.Sub(now)
		if remaining < 0 {
			remaining = 0
		}
		if remaining > c.TTL {
			remaining = c.TTL
		}
		if age := c.TTL - remaining; age > c.Timing.AgeHint {
			c.Timing.AgeHint = age
		}
		result = append(result, c)
	}
	sort.Slice(result, func(i, j int) bool { return keyString(result[i].Key) < keyString(result[j].Key) })
	return result, nil
}

// Fence discards an exact already-admitted payload but keeps its immutable source
// version and content hash. Older versions and same-version conflicts still fail.
// It is only for local storage selection after normal origin verification.
func (r *Replica) Fence(key Key, version Version) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[key]
	if !ok {
		return errors.New("fence has no admitted record")
	}
	if e.component.Version != version {
		return nil
	}
	e.component = Component{Key: key, Version: version}
	e.fenced = true
	r.entries[key] = e
	return nil
}
