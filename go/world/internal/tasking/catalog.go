package tasking

import (
	"sort"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
)

type CatalogEntry struct {
	Type  string              `json:"type"`
	Class mission.SafetyClass `json:"class"`
}
type Catalog struct {
	Assignee string         `json:"assignee"`
	Domain   mission.Domain `json:"domain"`
	RunID    string         `json:"run_id"`
	Types    []CatalogEntry `json:"types"`
}

// Catalog returns trusted configured types, not a caller-supplied safety class
// or evidence that a particular parameter document is valid for an adapter.
func (s *Store) Catalog() Catalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Catalog{Assignee: s.cfg.Assignee, Domain: s.cfg.Domain, RunID: s.cfg.RunID, Types: []CatalogEntry{}}
	for typ, class := range s.cfg.Catalog {
		out.Types = append(out.Types, CatalogEntry{Type: typ, Class: class})
	}
	sort.Slice(out.Types, func(i, j int) bool { return out.Types[i].Type < out.Types[j].Type })
	return out
}
