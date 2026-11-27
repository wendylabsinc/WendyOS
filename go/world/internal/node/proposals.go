package node

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
)

func (n *Node) configureProposals() error {
	if n.cfg.Proposals == nil {
		return nil
	}
	if n.cfg.Tasks == nil {
		return errors.New("proposals require a trusted local task catalog")
	}
	catalog := n.cfg.Tasks.Catalog()
	types := map[string]mission.SafetyClass{}
	for _, entry := range catalog.Types {
		types[entry.Type] = entry.Class
	}
	if catalog.Domain != mission.Simulation || !n.cfg.Proposals.MatchesScope(n.cfg.Tenant, n.cfg.Site, catalog.RunID, n.cfg.Audience, n.cfg.TaskAssignee) || !n.cfg.Proposals.CatalogMatches(types) {
		return errors.New("proposal store scope or catalog mismatch")
	}
	return nil
}

func proposalError(w http.ResponseWriter, err error) {
	code := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, proposals.ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, proposals.ErrScope):
		code = http.StatusForbidden
	case errors.Is(err, proposals.ErrAuthority):
		code = http.StatusUnauthorized
	case errors.Is(err, proposals.ErrConflict):
		code = http.StatusConflict
	case errors.Is(err, proposals.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, proposals.ErrCapacity):
		code = http.StatusInsufficientStorage
	}
	http.Error(w, err.Error(), code)
}

func (n *Node) proposals(w http.ResponseWriter, r *http.Request) {
	if n.cfg.Proposals == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		if _, ok := n.authorize(w, r, "read", nil); !ok {
			return
		}
		q, err := strictQuery(r, "limit", "after")
		if err != nil {
			proposalError(w, proposals.ErrInvalid)
			return
		}
		limit := 20
		if q["limit"] != "" {
			limit, err = strconv.Atoi(q["limit"])
			if err != nil {
				proposalError(w, proposals.ErrInvalid)
				return
			}
		}
		page, err := n.cfg.Proposals.List(limit, q["after"])
		if err != nil {
			proposalError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
		return
	}
	if !method(w, r, http.MethodPost) {
		return
	}
	if r.URL.RawQuery != "" {
		proposalError(w, proposals.ErrInvalid)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	id, ok := n.authorize(w, r, "propose", body)
	if !ok {
		return
	}
	if id.Kind != "agent" && id.Kind != "operator" {
		http.Error(w, "agent or operator proposer required", http.StatusForbidden)
		return
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(r.Header.Get(AuthorizationHeader))
	if err != nil {
		proposalError(w, proposals.ErrAuthority)
		return
	}
	proof, err := authority.DecodeRequest(raw)
	if err != nil {
		proposalError(w, proposals.ErrAuthority)
		return
	}
	validate := func() error { _, e := n.verifier.VerifyGrantContext(r.Context(), proof.Grant, "propose"); return e }
	record, created, err := n.cfg.Proposals.Submit(r.Context(), body, proof, validate)
	if err != nil {
		proposalError(w, err)
		return
	}
	view, err := n.cfg.Proposals.Get(record.Proposal.ID)
	if err != nil {
		proposalError(w, err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, view)
}

func (n *Node) proposalGet(w http.ResponseWriter, r *http.Request) {
	if n.cfg.Proposals == nil {
		http.NotFound(w, r)
		return
	}
	if !method(w, r, http.MethodGet) {
		return
	}
	if _, ok := n.authorize(w, r, "read", nil); !ok {
		return
	}
	id, ok := proposals.IDFromPath(r.URL.Path)
	if !ok || r.URL.RawQuery != "" {
		proposalError(w, proposals.ErrInvalid)
		return
	}
	view, err := n.cfg.Proposals.Get(id)
	if err != nil {
		proposalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func strictQuery(r *http.Request, keys ...string) (map[string]string, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, proposals.ErrInvalid
	}
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
	}
	out := map[string]string{}
	for key, list := range values {
		if !allowed[key] || len(list) != 1 || list[0] == "" {
			return nil, proposals.ErrInvalid
		}
		out[key] = list[0]
	}
	return out, err
}

func (n *Node) taskCatalog(w http.ResponseWriter, r *http.Request) {
	if !n.tasksEnabled(w) || !method(w, r, http.MethodGet) {
		return
	}
	if _, ok := n.authorize(w, r, "read", nil); !ok {
		return
	}
	if r.URL.RawQuery != "" {
		proposalError(w, proposals.ErrInvalid)
		return
	}
	catalog := n.cfg.Tasks.Catalog()
	encoded, err := json.Marshal(catalog)
	if err != nil || len(encoded) >= proposals.MaxPageBytes || len(catalog.Types) > 100 {
		http.Error(w, "catalog exceeds bound", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, catalog)
}
func (n *Node) taskStatus(w http.ResponseWriter, r *http.Request) {
	if !n.tasksEnabled(w) || !method(w, r, http.MethodGet) {
		return
	}
	if _, ok := n.authorize(w, r, "read", nil); !ok {
		return
	}
	q, err := strictQuery(r, "task_id")
	if err != nil || q["task_id"] == "" || len(q["task_id"]) > 256 {
		proposalError(w, proposals.ErrInvalid)
		return
	}
	record, err := n.cfg.Tasks.Get(q["task_id"])
	if err != nil {
		http.Error(w, "task unavailable", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, record)
}
