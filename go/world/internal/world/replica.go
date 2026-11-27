package world

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

var (
	ErrWrongScope       = errors.New("record does not belong to replica scope")
	ErrWrongSource      = errors.New("record source differs from authenticated publisher")
	ErrUnknownFreshness = errors.New("record needs a known clock bound in the replica clock domain")
	ErrFutureSample     = errors.New("record timestamp exceeds allowed future skew")
	ErrSynthetic        = errors.New("synthetic observations are not admitted by this replica")
	ErrConflict         = errors.New("same source version has different content")
	ErrCapacity         = errors.New("replica high-water capacity reached")
	ErrClockRegression  = errors.New("injected clock moved backwards")
)

type Version struct {
	Epoch    uint64 `json:"epoch"`
	Sequence uint64 `json:"sequence"`
}

func (v Version) Compare(other Version) int {
	if v.Epoch < other.Epoch {
		return -1
	}
	if v.Epoch > other.Epoch {
		return 1
	}
	if v.Sequence < other.Sequence {
		return -1
	}
	if v.Sequence > other.Sequence {
		return 1
	}
	return 0
}

type Key struct {
	Tenant string `json:"tenant"`
	Site   string `json:"site"`
	Entity string `json:"entity"`
	Type   string `json:"type"`
	Source Source `json:"source"`
}

// Component is a source-owned register. Tombstones remove only this exact key.
// Payload is opaque in this replica primitive; callers must validate its schema.
type Component struct {
	Key        Key             `json:"key"`
	Version    Version         `json:"version"`
	Frame      FrameRef        `json:"frame"`
	Timing     Timing          `json:"timing"`
	TTL        time.Duration   `json:"ttl_ns"`
	Provenance Provenance      `json:"provenance"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Tombstone  bool            `json:"tombstone"`
}
type Config struct {
	Tenant         string
	Site           string
	ClockDomain    string
	Now            func() time.Time
	MaxFutureSkew  time.Duration
	MaxEntries     int
	AllowSynthetic bool
	Frames         []FrameRef
}
type Result string

const (
	Applied   Result = "applied"
	Duplicate Result = "duplicate"
	Obsolete  Result = "obsolete"
)

type entry struct {
	fenced    bool
	component Component
	identity  string
	expires   time.Time
}

// Replica retains high-water marks even after expiry. It fails closed at
// MaxEntries rather than evicting those marks and accepting old records again.
type Replica struct {
	mu      sync.Mutex
	config  Config
	frames  map[string]string
	entries map[Key]entry
	lastNow time.Time
}

func NewReplica(config Config) (*Replica, error) {
	if config.Tenant == "" || config.Site == "" || config.ClockDomain == "" || config.Now == nil || config.MaxFutureSkew < 0 || config.MaxEntries <= 0 {
		return nil, errors.New("invalid replica configuration")
	}
	r := &Replica{config: config, frames: map[string]string{}, entries: map[Key]entry{}}
	for _, f := range config.Frames {
		if err := f.Validate(); err != nil {
			return nil, err
		}
		if _, exists := r.frames[f.ID]; exists {
			return nil, errors.New("duplicate configured frame")
		}
		r.frames[f.ID] = f.Revision
	}
	r.config.Frames = nil
	return r, nil
}
func (r *Replica) now() (time.Time, error) {
	n := r.config.Now()
	if n.IsZero() || (!r.lastNow.IsZero() && (n.Before(r.lastNow) || n.Round(0).Before(r.lastNow.Round(0)))) {
		return time.Time{}, ErrClockRegression
	}
	r.lastNow = n
	return n, nil
}
func (r *Replica) Apply(authenticated Source, c Component) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Sample times are cross-process assertions. A caller-local monotonic
	// reading must not change their age relative to the serialized timestamp.
	c.Timing.SampleTime = c.Timing.SampleTime.Round(0)
	if c.Key.Tenant != r.config.Tenant || c.Key.Site != r.config.Site {
		return "", ErrWrongScope
	}
	if c.Key.Source != authenticated {
		return "", ErrWrongSource
	}
	if err := authenticated.Validate(); err != nil {
		return "", err
	}
	if c.Key.Entity == "" || c.Key.Type == "" || c.Version.Epoch == 0 || c.Version.Sequence == 0 || c.TTL <= 0 {
		return "", errors.New("invalid component key, version or TTL")
	}
	if err := c.Frame.Validate(); err != nil {
		return "", err
	}
	if err := c.Timing.Validate(); err != nil {
		return "", err
	}
	if err := c.Provenance.Validate(); err != nil {
		return "", err
	}
	if !r.config.AllowSynthetic && c.Provenance.Synthetic() {
		return "", ErrSynthetic
	}
	if c.Provenance.Origin == Simulated && c.Provenance.Simulation.ClockDomain != c.Timing.ClockDomain {
		return "", errors.New("simulation clock domain mismatch")
	}
	if c.Tombstone {
		if len(c.Payload) != 0 {
			return "", errors.New("tombstone cannot carry payload")
		}
	} else if !json.Valid(c.Payload) {
		return "", errors.New("component payload must be valid JSON")
	}
	if !c.Timing.ErrorKnown || c.Timing.ClockDomain != r.config.ClockDomain {
		return "", ErrUnknownFreshness
	}
	now, err := r.now()
	if err != nil {
		return "", err
	}
	if c.Timing.SampleTime.After(now.Add(r.config.MaxFutureSkew)) {
		return "", ErrFutureSample
	}
	// Conservatively add clock uncertainty when estimating possible sample age.
	age := now.Sub(c.Timing.SampleTime)
	if age < 0 {
		age = 0
	}
	if c.Timing.ErrorBound > time.Duration(1<<63-1)-age {
		age = time.Duration(1<<63 - 1)
	} else {
		age += c.Timing.ErrorBound
	}
	if c.Timing.AgeHint > age {
		age = c.Timing.AgeHint
	}
	remaining := c.TTL - age
	if remaining < 0 {
		remaining = 0
	}
	expires := now.Add(remaining)
	// AgeHint is unsigned transit metadata, so it does not identify a version.
	identityComponent := c
	identityComponent.Timing.AgeHint = 0
	encoded, err := json.Marshal(identityComponent)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	identity := hex.EncodeToString(sum[:])
	if old, exists := r.entries[c.Key]; exists {
		order := c.Version.Compare(old.component.Version)
		if order < 0 {
			return Obsolete, nil
		}
		if order == 0 {
			if old.identity != identity {
				return "", ErrConflict
			}
			if expires.Before(old.expires) {
				old.expires = expires
				r.entries[c.Key] = old
			}
			return Duplicate, nil
		}
	} else if len(r.entries) >= r.config.MaxEntries {
		return "", ErrCapacity
	}
	r.entries[c.Key] = entry{component: normalizeComponent(c), identity: identity, expires: expires}
	return Applied, nil
}

// SetFrame changes which revision is visible. Mismatched records remain held
// in the replica and become visible only if their frame matches before expiry.
func (r *Replica) SetFrame(frame FrameRef) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames[frame.ID] = frame.Revision
	return nil
}

// MatchesFrame reports the currently active revision without activating it.
func (r *Replica) MatchesFrame(frame FrameRef) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return frame.Validate() == nil && r.frames[frame.ID] == frame.Revision
}
func (r *Replica) visible(e entry, now time.Time) bool {
	return !e.fenced && !e.component.Tombstone && now.Before(e.expires) && r.frames[e.component.Frame.ID] == e.component.Frame.Revision
}
func (r *Replica) Get(key Key) (Component, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now, err := r.now()
	if err != nil {
		return Component{}, false, err
	}
	e, ok := r.entries[key]
	if !ok || !r.visible(e, now) {
		return Component{}, false, nil
	}
	return cloneComponent(e.component), true, nil
}

// Snapshot returns only live records matching current frames, sorted by key.
func (r *Replica) Snapshot() ([]Component, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now, err := r.now()
	if err != nil {
		return nil, err
	}
	out := []Component{}
	for _, e := range r.entries {
		if r.visible(e, now) {
			out = append(out, cloneComponent(e.component))
		}
	}
	sortByKey(out, func(c Component) Key { return c.Key })
	return out, nil
}
func keyString(k Key) string { b, _ := json.Marshal(k); return string(b) }

// normalizeComponent owns and normalizes newly admitted input exactly as the
// persisted JSON format does. Retained records already passed this boundary.
func normalizeComponent(c Component) Component {
	b, err := json.Marshal(c)
	if err != nil {
		panic(fmt.Sprintf("validated component clone: %v", err))
	}
	var out Component
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

// cloneComponent copies an already-normalized retained record without encoding
// and parsing it again. Every mutable payload and ancestry pointer is owned.
func cloneComponent(c Component) Component {
	c.Payload = slices.Clone(c.Payload)
	c.Provenance.Simulation = cloneValue(c.Provenance.Simulation)
	c.Provenance.OriginalCaptureTime = cloneValue(c.Provenance.OriginalCaptureTime)
	c.Provenance.Inputs = slices.Clone(c.Provenance.Inputs)
	for i := range c.Provenance.Inputs {
		p := &c.Provenance.Inputs[i]
		p.Simulation = cloneValue(p.Simulation)
		p.OriginalCaptureTime = cloneValue(p.OriginalCaptureTime)
	}
	return c
}
func cloneValue[T any](v *T) *T {
	if v == nil {
		return nil
	}
	copy := *v
	return &copy
}
