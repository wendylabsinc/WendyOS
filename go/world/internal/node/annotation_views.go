package node

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Historical amendments suppress legacy original-note projection even after
// their grant expires/revokes. They never regain current authority by doing so.
func (n *Node) amendedOriginalsLocked() (map[world.Key]bool, error) {
	records, e := n.store.Records()
	if e != nil {
		return nil, e
	}
	out := map[world.Key]bool{}
	for _, c := range records {
		if !annotations.IsAmendment(c) {
			continue
		}
		a, e := annotations.ValidateAmendment(c)
		if e != nil {
			return nil, e
		}
		original, _, e := annotations.Original(a)
		if e != nil {
			return nil, e
		}
		out[original.Key] = true
	}
	return out, nil
}

type AnnotationView struct {
	OriginalSHA256          string         `json:"original_sha256"`
	OriginalKey             world.Key      `json:"original_key"`
	Frame                   world.FrameRef `json:"frame"`
	AuthorPrincipal         string         `json:"author_principal"`
	State                   string         `json:"state"`
	Text                    string         `json:"text"`
	PositionMM              [3]int64       `json:"position_mm"`
	HeadSHA256              string         `json:"head_sha256"`
	ExpiresAtUnixMS         int64          `json:"expires_at_unix_ms"`
	AmendmentSHA256         []string       `json:"amendment_sha256"`
	Conflict                bool           `json:"conflict"`
	VerificationUntilUnixMS int64          `json:"verification_until_unix_ms"`
	CurrentVerification     bool           `json:"current_verification"`
}
type AnnotationViewsRequest struct {
	Revision string `json:"revision"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}
type AnnotationViewsResponse struct {
	Views      []AnnotationView `json:"views"`
	Revision   string           `json:"revision"`
	NextOffset int              `json:"next_offset"`
	Complete   bool             `json:"complete"`
	ObservedAt time.Time        `json:"observed_at"`
	Scope      string           `json:"scope"`
}

// The caller holds the node mutex. Records came through admission or checked
// reopen. No obsolete record is re-admitted while producing historical labels.
func (n *Node) annotationViewsLocked(now time.Time) ([]AnnotationView, error) {
	records, e := n.store.Records()
	if e != nil {
		return nil, e
	}
	originals := map[string]json.RawMessage{}
	amends := map[string][]world.Component{}
	proofs := map[string]authority.SignedRecord{}
	addOriginal := func(raw json.RawMessage) { originals[annotations.SHA256(raw)] = append(json.RawMessage(nil), raw...) }
	for _, c := range records {
		if !annotations.Reserved(c) {
			continue
		}
		d, e := recordDigest(c)
		if e != nil {
			return nil, e
		}
		p, ok := n.proofs[d]
		if !ok {
			return nil, errors.New("annotation proof missing")
		}
		if annotations.IsAmendment(c) {
			a, e := annotations.ValidateAmendment(c)
			if e != nil {
				return nil, e
			}
			addOriginal(a.OriginalRecord)
			amends[a.OriginalSHA256] = append(amends[a.OriginalSHA256], c)
			proofs[annotations.SHA256(c.Payload)] = p
		} else {
			raw, _ := json.Marshal(p)
			addOriginal(raw)
		}
	}
	keys := make([]string, 0, len(originals))
	ownerRoots := map[world.Key][]string{}
	for id, raw := range originals {
		keys = append(keys, id)
		c, _, e := annotations.Original(annotations.Amendment{OriginalRecord: raw})
		if e != nil {
			return nil, e
		}
		ownerRoots[c.Key] = append(ownerRoots[c.Key], id)
	}
	sort.Strings(keys)
	out := make([]AnnotationView, 0, len(keys))
	for _, id := range keys {
		v, e := annotations.Resolve(originals[id], amends[id], now)
		if e != nil {
			return nil, e
		}
		_, note, _ := annotations.Original(annotations.Amendment{OriginalRecord: originals[id]})
		current := true
		until := time.UnixMilli(v.ExpiresAtUnixMS).Add(-v.Original.Timing.ErrorBound)
		var original authority.SignedRecord
		json.Unmarshal(originals[id], &original)
		// Original is historical within an amendment; its old grant is not borrowed
		// to authorize a correction. Without amendments, verify the original normally.
		if len(amends[id]) == 0 {
			identity, checkErr := n.verifier.VerifyRecord(original)
			current = checkErr == nil
			if current && identity.NotAfter.Before(until) {
				until = identity.NotAfter
			}
		} else {
			for _, c := range amends[id] {
				identity, checkErr := n.verifier.VerifyRecord(proofs[annotations.SHA256(c.Payload)])
				if checkErr != nil {
					current = false
				} else if identity.NotAfter.Before(until) {
					until = identity.NotAfter
				}
			}
		}
		if len(ownerRoots[v.Original.Key]) > 1 {
			v.Conflict = true
			v.State = "conflict"
			v.Text = ""
		}
		if !time.Now().Before(until) {
			current = false
		}
		if !current && v.State != "expired" {
			v.State = "historical_" + v.State
			v.Text = ""
		}
		if !now.Add(v.Original.Timing.ErrorBound).Before(time.UnixMilli(v.ExpiresAtUnixMS)) {
			v.State = "expired"
			v.Text = ""
			current = false
		}
		if len(v.AmendmentSHA256) > annotations.MaxAmendments {
			v.AmendmentSHA256 = v.AmendmentSHA256[:annotations.MaxAmendments]
		}
		out = append(out, AnnotationView{OriginalSHA256: id, OriginalKey: v.Original.Key, Frame: v.Original.Frame, AuthorPrincipal: note.AuthorPrincipal, State: v.State, Text: v.Text, PositionMM: v.PositionMM, HeadSHA256: v.HeadSHA256, ExpiresAtUnixMS: v.ExpiresAtUnixMS, AmendmentSHA256: v.AmendmentSHA256, Conflict: v.Conflict, VerificationUntilUnixMS: until.UnixMilli(), CurrentVerification: current})
	}
	return out, nil
}
func (n *Node) annotationViews(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	if _, ok = n.authorize(w, r, "read", raw); !ok {
		return
	}
	var q AnnotationViewsRequest
	if authority.DecodeStrict(raw, &q) != nil || q.Offset < 0 || q.Limit < 1 || q.Limit > 8 || len(q.Revision) > 64 || q.Offset > 0 && len(q.Revision) != 64 {
		http.Error(w, "invalid annotation view page", 400)
		return
	}
	n.lock()
	defer n.mu.Unlock()
	views, e := n.annotationViewsLocked(time.Now())
	if e != nil {
		http.Error(w, "annotation history unavailable", 503)
		return
	}
	for i := range views {
		if views[i].CurrentVerification && time.Now().UnixMilli() >= views[i].VerificationUntilUnixMS {
			views[i].CurrentVerification = false
			views[i].State = "historical_" + views[i].State
			views[i].Text = ""
		}
	}
	data, _ := json.Marshal(views)
	revision := annotations.SHA256(data)
	if q.Revision != "" && q.Revision != revision || q.Offset > len(views) {
		http.Error(w, "annotation view changed; restart page traversal", 409)
		return
	}
	end := min(len(views), q.Offset+q.Limit)
	response := AnnotationViewsResponse{Views: views[q.Offset:end], Revision: revision, NextOffset: end, Complete: end == len(views), ObservedAt: time.Now(), Scope: "local replica; amendment-aware readers required; no global completeness"}
	encoded, e := json.Marshal(response)
	if e != nil || len(encoded)+1 > MaxResponseBytes {
		http.Error(w, "annotation view exceeds response bound", 503)
		return
	}
	authorization, _ := base64.StdEncoding.Strict().DecodeString(r.Header.Get(AuthorizationHeader))
	var request authority.SignedRequest
	if authority.DecodeStrict(authorization, &request) != nil {
		http.Error(w, "invalid reader", 401)
		return
	}
	if _, e = n.verifier.VerifyGrantContext(r.Context(), request.Grant, "read"); e != nil || time.Now().UnixMilli() >= request.ExpiresAtUnixMS {
		http.Error(w, "reader expired", 401)
		return
	}
	for _, v := range response.Views {
		if v.CurrentVerification && time.Now().UnixMilli() >= v.VerificationUntilUnixMS {
			http.Error(w, "annotation validity crossed response boundary", 409)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(append(encoded, '\n'))
}
