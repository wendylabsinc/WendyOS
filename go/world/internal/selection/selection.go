// Package selection defines bounded, schema-independent World read selections.
// These filters narrow an authorized read; they never grant access or select
// which records another peer is allowed to replicate.
package selection

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const MaxValues = 32
const MaxRequestBytes = 64 << 10
const MaxCursorBytes = 4096
const DefaultPageSize = 64
const MaxPageSize = 128
const DefaultPageBytes = 256 << 10
const MaxPageBytes = (1 << 20) - 1
const MaxWaitMS = 5000

type Selection struct {
	Region   *Region          `json:"region,omitempty"`
	Types    []string         `json:"types,omitempty"`
	Entities []string         `json:"entities,omitempty"`
	Sources  []world.Source   `json:"sources,omitempty"`
	Frames   []world.FrameRef `json:"frames,omitempty"`
}

type Request struct {
	Version   int       `json:"version"`
	Selection Selection `json:"selection"`
	Cursor    string    `json:"cursor,omitempty"`
	PageSize  int       `json:"page_size,omitempty"`
	MaxBytes  int       `json:"max_bytes,omitempty"`
}

type WatchRequest struct {
	Version   int       `json:"version"`
	Selection Selection `json:"selection"`
	Revision  string    `json:"revision,omitempty"`
	WaitMS    int       `json:"wait_ms"`
	PageSize  int       `json:"page_size,omitempty"`
	MaxBytes  int       `json:"max_bytes,omitempty"`
}

// ReplicaScope describes retained knowledge, not read authority or global completeness.
type ReplicaScope struct {
	UnknownLocation string    `json:"unknown_location,omitempty"`
	Partial         bool      `json:"partial"`
	Tenant          string    `json:"tenant"`
	Site            string    `json:"site"`
	PolicySHA256    string    `json:"policy_sha256"`
	Selection       Selection `json:"selection"`
	RequiredTypes   []string  `json:"required_types"`
}

type Response struct {
	Spatial      []Spatial         `json:"spatial,omitempty"`
	ReplicaScope *ReplicaScope     `json:"replica_scope,omitempty"`
	Version      int               `json:"version"`
	Components   []world.Component `json:"components"`
	Revision     string            `json:"revision"`
	NextCursor   string            `json:"next_cursor,omitempty"`
	Complete     bool              `json:"complete"`
	ObservedAt   time.Time         `json:"observed_at"`
	TimedOut     bool              `json:"timed_out,omitempty"`
}

type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Restart bool   `json:"restart,omitempty"`
}

func bounded(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) && strings.TrimSpace(value) == value
}
func canonical(v any) string { b, _ := json.Marshal(v); return string(b) }

func (s Selection) Normalize() (Selection, error) {
	out := Selection{Types: append([]string(nil), s.Types...), Entities: append([]string(nil), s.Entities...), Sources: append([]world.Source(nil), s.Sources...), Frames: append([]world.FrameRef(nil), s.Frames...)}
	for _, values := range [][]string{out.Types, out.Entities} {
		if len(values) > MaxValues {
			return Selection{}, errors.New("too many selection values")
		}
		sort.Strings(values)
		for i, v := range values {
			if !bounded(v) || (i > 0 && values[i-1] == v) {
				return Selection{}, errors.New("selection values must be bounded, nonempty and distinct")
			}
		}
	}
	if len(out.Sources) > MaxValues || len(out.Frames) > MaxValues {
		return Selection{}, errors.New("too many source or frame selectors")
	}
	sort.Slice(out.Sources, func(i, j int) bool { return canonical(out.Sources[i]) < canonical(out.Sources[j]) })
	for i, v := range out.Sources {
		if v.Validate() != nil || !bounded(v.Device) || !bounded(v.App) || !bounded(v.Service) || !bounded(v.Stream) || (i > 0 && out.Sources[i-1] == v) {
			return Selection{}, errors.New("invalid or duplicate exact source")
		}
	}
	sort.Slice(out.Frames, func(i, j int) bool { return canonical(out.Frames[i]) < canonical(out.Frames[j]) })
	for i, v := range out.Frames {
		if !bounded(v.ID) || !bounded(v.Revision) || (i > 0 && out.Frames[i-1] == v) {
			return Selection{}, errors.New("invalid or duplicate exact frame revision")
		}
	}
	if s.Region != nil {
		r := *s.Region
		if err := r.Validate(); err != nil {
			return Selection{}, err
		}
		out.Region = &r
		if len(out.Types) == 0 || len(out.Frames) != 0 {
			return Selection{}, errors.New("region requires explicit supported types and its own exact frame, without frames selector")
		}
		for _, typ := range out.Types {
			if !SpatialType(typ) {
				return Selection{}, errors.New("unsupported spatial type")
			}
		}
	}
	return out, nil
}

func (s Selection) Matches(c world.Component) bool {
	if len(s.Types) > 0 && !contains(s.Types, c.Key.Type) {
		return false
	}
	if len(s.Entities) > 0 && !contains(s.Entities, c.Key.Entity) {
		return false
	}
	if len(s.Sources) > 0 {
		found := false
		for _, source := range s.Sources {
			if source == c.Key.Source {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(s.Frames) > 0 {
		found := false
		for _, frame := range s.Frames {
			if frame == c.Frame {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if s.Region != nil {
		return s.Region.Classify(c).Class != "outside"
	}
	return true
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (r Request) Normalize() (Request, error) {
	if r.Version == 0 {
		r.Version = 1
	}
	if r.PageSize == 0 {
		r.PageSize = DefaultPageSize
	}
	if r.MaxBytes == 0 {
		r.MaxBytes = DefaultPageBytes
	}
	if r.Version != 1 || r.PageSize < 1 || r.PageSize > MaxPageSize || r.MaxBytes < 4096 || r.MaxBytes > MaxPageBytes || len(r.Cursor) > MaxCursorBytes {
		return Request{}, errors.New("invalid query version, page bounds or cursor")
	}
	var err error
	r.Selection, err = r.Selection.Normalize()
	if err != nil {
		return Request{}, err
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > MaxRequestBytes {
		return Request{}, errors.New("selection request exceeds byte bound")
	}
	return r, nil
}

func (r WatchRequest) Normalize() (WatchRequest, error) {
	page, err := (Request{Version: r.Version, Selection: r.Selection, PageSize: r.PageSize, MaxBytes: r.MaxBytes}).Normalize()
	if err != nil {
		return WatchRequest{}, err
	}
	if r.WaitMS < 0 || r.WaitMS > MaxWaitMS || len(r.Revision) > MaxCursorBytes {
		return WatchRequest{}, errors.New("invalid watch wait or revision")
	}
	r.Version, r.Selection, r.PageSize, r.MaxBytes = page.Version, page.Selection, page.PageSize, page.MaxBytes
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > MaxRequestBytes {
		return WatchRequest{}, errors.New("watch request exceeds byte bound")
	}
	return r, nil
}

func Digest(value any) string {
	sum := sha256.Sum256([]byte(canonical(value)))
	return hex.EncodeToString(sum[:])
}
func Key(c world.Component) string { return Digest(c.Key) }

func ValidateResponse(r Response, request Request) error {
	if r.Version != 1 || r.Components == nil || len(r.Components) > request.PageSize || r.Revision == "" || len(r.Revision) > MaxCursorBytes || len(r.NextCursor) > MaxCursorBytes || r.Complete != (r.NextCursor == "") || r.ObservedAt.IsZero() {
		return errors.New("invalid selected-query response")
	}
	if request.Selection.Region != nil && len(r.Spatial) != len(r.Components) || request.Selection.Region == nil && len(r.Spatial) != 0 {
		return errors.New("spatial descriptors differ from selection")
	}
	previous := ""
	for i, component := range r.Components {
		key := Key(component)
		if !request.Selection.Matches(component) || component.Tombstone || key <= previous {
			return errors.New("selected-query response has unrelated, deleted, duplicate or unordered records")
		}
		if request.Selection.Region != nil {
			want := request.Selection.Region.Classify(component)
			got := r.Spatial[i]
			if component.TTL <= 0 || component.Timing.AgeHint < 0 || component.Timing.AgeHint >= component.TTL || got.ExpiresAt.After(r.ObservedAt.Add(component.TTL-component.Timing.AgeHint)) || got.ContentSHA256 != Digest(component) || got.Key != key || got.Class != want.Class || got.Reason != want.Reason || Digest(got.PositionM) != Digest(want.PositionM) || Digest(got.UncertaintyM) != Digest(want.UncertaintyM) || !got.ExpiresAt.After(r.ObservedAt) {
				return errors.New("invalid spatial descriptor")
			}
		}
		previous = key
	}
	if !r.Complete && len(r.Components) == 0 {
		return errors.New("empty incomplete query page")
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw)+1 > request.MaxBytes {
		return errors.New("selected-query response exceeds requested byte bound")
	}
	return nil
}
