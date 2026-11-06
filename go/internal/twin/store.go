// Package twin owns the persisted, shared room state on Linux.
package twin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
)

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}
type Site struct {
	Version            int               `json:"version"`
	ID                 string            `json:"id"`
	Name               string            `json:"name"`
	ModelRevision      string            `json:"modelRevision"`
	MetresPerModelUnit float64           `json:"metresPerModelUnit"`
	Cameras            []json.RawMessage `json:"cameras"`
}
type Room struct {
	HasModel     bool                         `json:"hasModel"`
	HasVisualMap bool                         `json:"hasVisualMap"`
	FloorTiles   []Point                      `json:"floorTiles,omitempty"`
	References   map[string][]sitenav.Surface `json:"references,omitempty"`
	Site         Site                         `json:"site"`
	Revision     uint64                       `json:"revision"`
	Model        []byte                       `json:"model,omitempty"`
	VisualMap    json.RawMessage              `json:"visualMap,omitempty"`
	Points       []Point                      `json:"points,omitempty"`
	Removed      bool                         `json:"removed"`
	Sources      map[string]uint64            `json:"sources,omitempty"`
}
type Store struct {
	mu        sync.Mutex
	directory string
}

func NewStore(directory string) (*Store, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	return &Store{directory: directory}, nil
}
func validID(id string) bool { _, e := uuid.Parse(id); return e == nil && len(id) == 36 }
func (s *Store) path(id string) string {
	return filepath.Join(s.directory, strings.ToLower(id)+".json")
}
func (s *Store) read(id string) (Room, error) {
	var r Room
	if !validID(id) {
		return r, errors.New("invalid room ID")
	}
	b, e := os.ReadFile(s.path(id))
	if e != nil {
		return r, e
	}
	e = json.Unmarshal(b, &r)
	return r, e
}
func (s *Store) State(id string) (Room, error) {
	s.mu.Lock()
	r, e := s.read(id)
	s.mu.Unlock()
	if e == nil {
		r.FloorTiles = FloorTiles(r.Points)
	}
	return r, e
}
func (s *Store) asset(id, suffix string) string {
	return filepath.Join(s.directory, strings.ToLower(id)+suffix)
}
func (s *Store) assets(r Room) (Room, error) {
	var e error
	if r.HasModel {
		r.Model, e = os.ReadFile(s.asset(r.Site.ID, ".usdz"))
		if e != nil {
			return r, e
		}
	}
	if r.HasVisualMap {
		r.VisualMap, e = os.ReadFile(s.asset(r.Site.ID, ".visual-map"))
		if e != nil {
			return r, e
		}
	}
	return r, nil
}
func (s *Store) Get(id string) (Room, error) {
	r, e := s.State(id)
	if e != nil {
		return r, e
	}
	return s.assets(r)
}
func (s *Store) writeAsset(id, suffix string, data []byte) error {
	f, e := os.CreateTemp(s.directory, ".asset-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(data); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), s.asset(id, suffix))
}

func (s *Store) List() ([]Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := []Room{}
	entries, e := os.ReadDir(s.directory)
	if e != nil {
		return nil, e
	}
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		id := name[:len(name)-5]
		if !validID(id) {
			continue
		}
		r, e := s.read(id)
		if e != nil {
			return nil, e
		}
		r.Model = nil
		r.VisualMap = nil
		r.Points = nil
		r.References = nil
		r.Sources = nil
		result = append(result, r)
	}
	return result, nil
}

var ErrConflict = errors.New("room changed; refresh before editing")

func (s *Store) write(r Room) error {
	r.FloorTiles = nil
	r.Model = nil
	r.VisualMap = nil
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(s.directory, ".room-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if e = os.Rename(f.Name(), s.path(r.Site.ID)); e != nil {
		return e
	}
	d, e := os.Open(s.directory)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (s *Store) Put(r Room) (Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := validateSite(r.Site); e != nil {
		return r, e
	}
	if len(r.Model) > 128<<20 || (len(r.Model) > 0 && (len(r.Model) < 4 || string(r.Model[:4]) != "PK\x03\x04")) {
		return r, errors.New("invalid model")
	}
	old, e := s.read(r.Site.ID)
	if e != nil && !os.IsNotExist(e) {
		return r, e
	}
	if e == nil && old.Revision != r.Revision || os.IsNotExist(e) && r.Revision != 0 {
		return r, ErrConflict
	}
	// Clients cannot rewrite robot observations, sequence guards, or geometry frames.
	if e == nil {
		if !strings.EqualFold(old.Site.ModelRevision, r.Site.ModelRevision) {
			return r, errors.New("create a new room for a different coordinate frame")
		}
		old, e = s.assets(old)
		if e != nil {
			return r, e
		}
		if r.Model == nil {
			r.Model = old.Model
		}
		if r.VisualMap == nil {
			r.VisualMap = old.VisualMap
		}
		if !bytes.Equal(r.Model, old.Model) || !bytes.Equal(r.VisualMap, old.VisualMap) || r.Site.MetresPerModelUnit != old.Site.MetresPerModelUnit {
			return r, errors.New("room geometry and scale are immutable; contribute registered observations or create a new room")
		}
		r.HasModel = old.HasModel
		r.HasVisualMap = old.HasVisualMap
		r.References = old.References
		r.Points = old.Points
		r.Sources = old.Sources
	} else {
		r.HasModel = len(r.Model) > 0
		r.HasVisualMap = len(r.VisualMap) > 0
		if r.HasModel {
			if e = s.writeAsset(r.Site.ID, ".usdz", r.Model); e != nil {
				return r, e
			}
		}
		if r.HasVisualMap {
			if !r.HasModel || len(r.VisualMap) > 16<<20 {
				return r, errors.New("invalid visual map")
			}
			if e = s.writeAsset(r.Site.ID, ".visual-map", r.VisualMap); e != nil {
				return r, e
			}
		}
		r.Points = nil
		r.References = nil
		r.Sources = nil
	}
	r.Revision++
	r.FloorTiles = FloorTiles(r.Points)
	return r, s.write(r)
}

type Contribution struct {
	Transform     []float64 `json:"transform,omitempty"`
	Source        string    `json:"source"`
	Sequence      uint64    `json:"sequence"`
	ModelRevision string    `json:"modelRevision"`
	// Points are Y-up metres; Transform optionally registers the scan to the room.
	Points []Point `json:"points"`
}

func (s *Store) Contribute(id string, c Contribution) error {
	if len(c.Transform) != 0 && len(c.Transform) != 4 {
		return errors.New("invalid scan registration")
	}
	for _, v := range c.Transform {
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 10000 {
			return errors.New("invalid scan registration")
		}
	}
	if len(c.Transform) == 4 {
		t := c.Transform
		cs, sn := math.Cos(t[3]), math.Sin(t[3])
		for i, p := range c.Points {
			c.Points[i] = Point{cs*p.X + sn*p.Z + t[0], p.Y + t[1], -sn*p.X + cs*p.Z + t[2]}
		}
	}
	if len(c.Source) == 0 || len(c.Source) > 200 || c.Sequence == 0 || len(c.Points) == 0 || len(c.Points) > 50000 {
		return errors.New("invalid contribution")
	}
	for _, p := range c.Points {
		for _, v := range []float64{p.X, p.Y, p.Z} {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 10000 {
				return errors.New("invalid point")
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, e := s.read(id)
	if e != nil {
		return e
	}
	if r.Removed || !strings.EqualFold(r.Site.ModelRevision, c.ModelRevision) {
		return errors.New("room frame mismatch")
	}
	if r.Sources == nil {
		r.Sources = map[string]uint64{}
	}
	if c.Sequence <= r.Sources[c.Source] {
		return ErrConflict
	}
	if len(r.Sources) >= 256 && r.Sources[c.Source] == 0 {
		return errors.New("too many contributors")
	}
	// Ten-centimetre voxels bound persisted geometry. This map is historical
	// rendering evidence; it never substitutes for fresh navigation observations.
	voxels := map[[3]int]Point{}
	for _, p := range append(r.Points, c.Points...) {
		key := [3]int{int(math.Floor(p.X * 10)), int(math.Floor(p.Y * 10)), int(math.Floor(p.Z * 10))}
		voxels[key] = p
	}
	if len(voxels) > 200000 {
		return fmt.Errorf("room exceeds 200000 voxels")
	}
	r.Points = make([]Point, 0, len(voxels))
	for _, p := range voxels {
		r.Points = append(r.Points, p)
	}
	r.Sources[c.Source] = c.Sequence
	r.Revision++
	return s.write(r)
}

func (s *Store) Frame(frame string) (Room, error) {
	parts := strings.Split(frame, "/")
	if len(parts) != 2 {
		return Room{}, errors.New("invalid room frame")
	}
	s.mu.Lock()
	r, e := s.read(parts[0])
	s.mu.Unlock()
	if e != nil {
		return r, e
	}
	if r.Removed || !strings.EqualFold(r.Site.ModelRevision, parts[1]) {
		return r, errors.New("room frame changed")
	}
	return r, nil
}

// Confirmed references are room state, shared between operators and persisted.
func (s *Store) SaveReferences(frame, serial string, points []sitenav.Surface) error {
	r, e := s.Frame(frame)
	if e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, e = s.read(r.Site.ID)
	if e != nil {
		return e
	}
	if r.References == nil {
		r.References = map[string][]sitenav.Surface{}
	}
	cells := map[[3]int]sitenav.Surface{}
	for _, p := range append(r.References[serial], points...) {
		key := [3]int{int(math.Round(p.Position.X * 10)), int(math.Round(p.Position.Y * 10)), int(math.Round(p.Position.Z * 10))}
		if old, ok := cells[key]; ok && old.Stable && !p.Stable {
			continue
		}
		cells[key] = p
	}
	merged := make([]sitenav.Surface, 0, len(cells))
	for _, p := range cells {
		merged = append(merged, p)
	}
	sort.Slice(merged, func(i, j int) bool {
		a, b := merged[i].Position, merged[j].Position
		if a.X != b.X {
			return a.X < b.X
		}
		if a.Y != b.Y {
			return a.Y < b.Y
		}
		return a.Z < b.Z
	})
	if len(merged) > 3000 {
		sample := make([]sitenav.Surface, 3000)
		for i := range sample {
			sample[i] = merged[i*len(merged)/3000]
		}
		merged = sample
	}
	r.References[serial] = merged
	r.Revision++
	return s.write(r)
}
