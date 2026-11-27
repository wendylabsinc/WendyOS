package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// ComponentReadConfig commissions a read-only simulation endpoint. The callback
// owns source authorization, freshness and schema checks, and returns owned
// components. No record is admitted to a standalone replica or signed here.
type ComponentReadConfig struct {
	Tenant, Site, Audience string
	MaxRecords             int
	RequestTimeout         time.Duration
}

// NewComponentReadHandler exposes only GET /v1/components using the existing
// simulation read protocol. Missing site, task and evidence services return 404;
// callers must not interpret those responses as empty replicas or clearance.
func NewComponentReadHandler(cfg ComponentReadConfig, verifier *authority.Verifier, read func(context.Context) ([]world.Component, error)) (http.Handler, error) {
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	if cfg.Tenant == "" || cfg.Site == "" || cfg.Audience == "" || cfg.MaxRecords < 1 || cfg.MaxRecords > 256 || cfg.RequestTimeout < time.Millisecond || cfg.RequestTimeout > 10*time.Second || read == nil || !verifier.MatchesScope(cfg.Tenant, cfg.Site, "simulation", cfg.Audience) {
		return nil, errors.New("explicit simulation reader configuration required")
	}
	// Reuse request verification without constructing a mutable replica/store.
	n := &Node{cfg: Config{Tenant: cfg.Tenant, Site: cfg.Site, Audience: cfg.Audience}, verifier: verifier}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path != "/v1/components" || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
			http.NotFound(w, r)
			return
		}
		if !method(w, r, http.MethodGet) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), cfg.RequestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		if _, ok := n.authorize(w, r, "read", nil); !ok {
			return
		}
		components, err := read(ctx)
		if err != nil || ctx.Err() != nil {
			http.Error(w, "native replica unavailable", http.StatusServiceUnavailable)
			return
		}
		response, err := componentReadResponse(cfg, components)
		if err != nil {
			http.Error(w, "native snapshot unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}), nil
}

func componentReadResponse(cfg ComponentReadConfig, components []world.Component) (QueryResponse, error) {
	if components == nil || len(components) > cfg.MaxRecords {
		return QueryResponse{}, errors.New("incomplete or oversized component read")
	}
	// Sort a private slice; do not mutate the callback's order or its records.
	visible := append(make([]world.Component, 0, len(components)), components...)
	keys := make(map[world.Key]string, len(visible))
	for _, c := range visible {
		if c.Key.Tenant != cfg.Tenant || c.Key.Site != cfg.Site || c.Key.Entity == "" || c.Key.Type == "" || c.Key.Source.Validate() != nil || c.Frame.Validate() != nil || c.Timing.Validate() != nil || c.Provenance.Validate() != nil || !c.Provenance.Synthetic() || !strings.HasPrefix(c.Timing.ClockDomain, "simulation:") || c.Version.Epoch == 0 || c.Version.Sequence == 0 || c.Tombstone || c.TTL <= 0 || !json.Valid(c.Payload) {
			return QueryResponse{}, errors.New("component scope or shape mismatch")
		}
		if _, exists := keys[c.Key]; exists {
			return QueryResponse{}, errors.New("duplicate component key")
		}
		key, err := json.Marshal(c.Key)
		if err != nil {
			return QueryResponse{}, err
		}
		keys[c.Key] = string(key)
	}
	sort.Slice(visible, func(i, j int) bool { return keys[visible[i].Key] < keys[visible[j].Key] })
	raw, err := json.Marshal(visible)
	if err != nil || len(raw) > MaxResponseBytes-256 {
		return QueryResponse{}, errors.New("component response exceeds budget")
	}
	digest := sha256.Sum256(raw)
	return QueryResponse{Components: visible, Cursor: hex.EncodeToString(digest[:])}, nil
}
