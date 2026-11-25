package node

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const selectedCursorLifetime = 2 * time.Minute
const maxSelectedWatchers = 16

var errSelectedChanged = errors.New("selected view changed; restart pagination")
var errSelectedExpired = errors.New("selection cursor expired; restart without it")
var errSelectedCursor = errors.New("invalid selection cursor or reader binding")
var errSelectedOversize = errors.New("one selected record exceeds the requested page byte limit")
var errSelectedAuthority = errors.New("reader authority expired or revoked")

type selectedAccess struct {
	identity authority.Identity
	request  authority.SignedRequest
}
type selectedCursor struct {
	Version   int    `json:"version"`
	Purpose   string `json:"purpose"`
	Tenant    string `json:"tenant"`
	Site      string `json:"site"`
	Principal string `json:"principal"`
	Key       string `json:"key"`
	Grant     string `json:"grant"`
	Selection string `json:"selection"`
	View      string `json:"view"`
	After     string `json:"after,omitempty"`
	Expires   int64  `json:"expires_unix_ms"`
}
type selectedRecord struct {
	component      world.Component
	key            string
	digest         string
	proof          string
	expires        time.Time
	authorityUntil time.Time
}

func (n *Node) selectedAuthorization(w http.ResponseWriter, r *http.Request, body []byte) (selectedAccess, bool) {
	id, ok := n.authorize(w, r, "read", body)
	if !ok {
		return selectedAccess{}, false
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(r.Header.Get(AuthorizationHeader))
	var request authority.SignedRequest
	if err != nil || authority.DecodeStrict(raw, &request) != nil {
		http.Error(w, "invalid read authorization", 401)
		return selectedAccess{}, false
	}
	return selectedAccess{id, request}, true
}

func (n *Node) selectedGrant(ctx context.Context, a selectedAccess) error {
	if _, err := n.verifier.VerifyGrantContext(ctx, a.request.Grant, "read"); err != nil {
		return errSelectedAuthority
	}
	if !time.Now().Before(time.UnixMilli(a.request.ExpiresAtUnixMS)) {
		return errSelectedAuthority
	}
	return nil
}

func (n *Node) encodeSelected(c selectedCursor) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, n.repairKey[:])
	mac.Write([]byte("world.selected-read.v1\x00"))
	mac.Write(raw)
	token := base64.RawURLEncoding.EncodeToString(append(raw, mac.Sum(nil)...))
	if len(token) > selection.MaxCursorBytes {
		return "", errSelectedCursor
	}
	return token, nil
}

func (n *Node) decodeSelected(token, purpose string, s selection.Selection, a selectedAccess) (selectedCursor, error) {
	var c selectedCursor
	if token == "" || len(token) > selection.MaxCursorBytes {
		return c, errSelectedCursor
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) <= sha256.Size {
		return c, errSelectedCursor
	}
	data, signature := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	mac := hmac.New(sha256.New, n.repairKey[:])
	mac.Write([]byte("world.selected-read.v1\x00"))
	mac.Write(data)
	if !hmac.Equal(signature, mac.Sum(nil)) || authority.DecodeStrictLimit(data, &c, selection.MaxCursorBytes) != nil {
		return c, errSelectedCursor
	}
	if c.Version != 1 || c.Purpose != purpose || c.Tenant != n.cfg.Tenant || c.Site != n.cfg.Site || c.Principal != a.identity.Principal || c.Key != a.identity.SigningKeySHA256 || c.Grant != a.identity.GrantID || c.Selection != selection.Digest(s) || !selectedHash(c.View) || (purpose == "page" && !selectedHash(c.After)) || (purpose == "view" && c.After != "") {
		return c, errSelectedCursor
	}
	if time.Now().UnixMilli() >= c.Expires {
		return c, errSelectedExpired
	}
	return c, nil
}
func selectedHash(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == value
}

func (n *Node) selectedFailure(w http.ResponseWriter, err error) {
	code, status, restart := "unavailable", http.StatusServiceUnavailable, false
	switch {
	case errors.Is(err, errSelectedCursor):
		code, status = "invalid_cursor", 400
	case errors.Is(err, errSelectedExpired):
		code, status, restart = "cursor_expired", 410, true
	case errors.Is(err, errSelectedChanged):
		code, status, restart = "view_changed", 409, true
	case errors.Is(err, errSelectedOversize):
		code, status = "record_too_large", 413
	case errors.Is(err, errSelectedAuthority):
		code, status = "reader_denied", 401
	}
	message := err.Error()
	if status == 503 {
		message = "selected replica view is unavailable"
	}
	writeJSON(w, status, selection.Failure{Code: code, Message: message, Restart: restart})
}

func (n *Node) selectedQuery(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	a, ok := n.selectedAuthorization(w, r, body)
	if !ok {
		return
	}
	var request selection.Request
	if err := authority.DecodeStrictLimit(body, &request, selection.MaxRequestBytes); err != nil {
		http.Error(w, "invalid selected query", 400)
		return
	}
	request, err := request.Normalize()
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var prior *selectedCursor
	if request.Cursor != "" {
		value, err := n.decodeSelected(request.Cursor, "page", request.Selection, a)
		if err != nil {
			n.selectedFailure(w, err)
			return
		}
		prior = &value
	}
	response, err := n.selectedPage(r.Context(), request, a, prior, false)
	if err != nil {
		n.selectedFailure(w, err)
		return
	}
	writeJSON(w, 200, response)
}

func (n *Node) selectedWatch(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	a, ok := n.selectedAuthorization(w, r, body)
	if !ok {
		return
	}
	var request selection.WatchRequest
	if err := authority.DecodeStrictLimit(body, &request, selection.MaxRequestBytes); err != nil {
		http.Error(w, "invalid selected watch", 400)
		return
	}
	request, err := request.Normalize()
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var prior *selectedCursor
	if request.Revision != "" {
		value, err := n.decodeSelected(request.Revision, "view", request.Selection, a)
		if err != nil {
			n.selectedFailure(w, err)
			return
		}
		prior = &value
	}
	if n.selectedWatchers.Add(1) > maxSelectedWatchers {
		n.selectedWatchers.Add(-1)
		writeJSON(w, 429, selection.Failure{Code: "watch_capacity", Message: "selected watch capacity reached"})
		return
	}
	defer n.selectedWatchers.Add(-1)
	deadline := time.Now().Add(time.Duration(request.WaitMS) * time.Millisecond)
	deadline = minSelectedTime(deadline, a.identity.NotAfter)
	deadline = minSelectedTime(deadline, time.UnixMilli(a.request.ExpiresAtUnixMS))
	if prior != nil {
		deadline = minSelectedTime(deadline, time.UnixMilli(prior.Expires))
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	timedOut := request.WaitMS == 0
	for {
		if prior != nil && time.Now().UnixMilli() >= prior.Expires {
			n.selectedFailure(w, errSelectedExpired)
			return
		}
		page := selection.Request{Version: 1, Selection: request.Selection, PageSize: request.PageSize, MaxBytes: request.MaxBytes}
		response, err := n.selectedPage(r.Context(), page, a, prior, true)
		if err != nil {
			n.selectedFailure(w, err)
			return
		}
		current, err := n.decodeSelected(response.Revision, "view", request.Selection, a)
		if err != nil {
			n.selectedFailure(w, err)
			return
		}
		changed := prior == nil || current.View != prior.View
		if changed || timedOut {
			response.TimedOut = !changed
			writeJSON(w, 200, response)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			timedOut = true
		case <-ticker.C:
		}
	}
}
func minSelectedTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func selectedVisible(e world.CheckpointEntry, cp world.Checkpoint, s selection.Selection) bool {
	if e.Component.Tombstone || !cp.CapturedAt.Before(e.ExpiresAt) || !s.Matches(e.Component) {
		return false
	}
	for _, frame := range cp.Frames {
		if frame == e.Component.Frame {
			return true
		}
	}
	return false
}

// Membership is checked against a fresh, durably observed checkpoint after
// origin verification. n.mu keeps normal publication/repair/revocation paths
// serialized. Pagination fails if the selected content or expiry caps change.
func (n *Node) selectedPage(ctx context.Context, request selection.Request, a selectedAccess, prior *selectedCursor, watch bool) (selection.Response, error) {
	if err := ctx.Err(); err != nil {
		return selection.Response{}, err
	}
	if err := n.lockContext(ctx); err != nil {
		return selection.Response{}, err
	}
	defer n.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return selection.Response{}, err
	}
	if err := n.selectedGrant(ctx, a); err != nil {
		return selection.Response{}, err
	}
	cp, err := n.store.RepairSnapshot()
	if err != nil {
		return selection.Response{}, err
	}
	// Apply the same immutable amendment projection as the legacy query. A
	// selection may exclude amendment records, but must not revive an original
	// note whose correction or withdrawal is already retained by this replica.
	// Historical amendments still suppress originals after their grant expires.
	amended, err := n.amendedOriginalsLocked()
	if err != nil {
		return selection.Response{}, err
	}
	verified := map[string]selectedRecord{}
	considered := map[string]bool{}
	for _, entry := range cp.Entries {
		if amended[entry.Component.Key] || !selectedVisible(entry, cp, request.Selection) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return selection.Response{}, err
		}
		considered[selection.Key(entry.Component)] = true
		d, err := recordDigest(entry.Component)
		if err != nil {
			return selection.Response{}, err
		}
		proof, ok := n.proofs[d]
		if !ok {
			return selection.Response{}, errors.New("selected record has no origin proof")
		}
		id, err := n.verifyRecordContext(ctx, proof)
		if errors.Is(err, authority.ErrDenied) {
			continue
		}
		if err != nil {
			return selection.Response{}, err
		}
		key := selection.Key(entry.Component)
		verified[key] = selectedRecord{key: key, digest: d, proof: selection.Digest(proof), authorityUntil: id.NotAfter}
	}
	// Reobserve expiry/frame state after potentially blocking proof checks.
	cp, err = n.store.RepairSnapshot()
	if err != nil {
		return selection.Response{}, err
	}
	rows := make([]selectedRecord, 0, len(verified))
	earliest := a.identity.NotAfter
	for _, entry := range cp.Entries {
		if amended[entry.Component.Key] || !selectedVisible(entry, cp, request.Selection) {
			continue
		}
		key := selection.Key(entry.Component)
		row, ok := verified[key]
		if !considered[key] {
			return selection.Response{}, errSelectedChanged
		}
		if !ok {
			continue
		}
		d, err := recordDigest(entry.Component)
		if err != nil {
			return selection.Response{}, err
		}
		if d != row.digest {
			return selection.Response{}, errSelectedChanged
		}
		row.component, row.expires = entry.Component, entry.ExpiresAt.Round(0)
		// Relay age can only become more conservative; original measurements and
		// provenance are unchanged. The stable revision binds the absolute cap.
		age := row.component.TTL - entry.ExpiresAt.Sub(cp.CapturedAt)
		if age > row.component.Timing.AgeHint {
			row.component.Timing.AgeHint = age
		}
		rows = append(rows, row)
		earliest = minSelectedTime(earliest, row.authorityUntil)
		if request.Selection.Region != nil {
			earliest = minSelectedTime(earliest, row.expires)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })
	type descriptor struct {
		Key     string    `json:"key"`
		Content string    `json:"content"`
		Proof   string    `json:"proof"`
		Expires time.Time `json:"expires"`
	}
	descriptors := make([]descriptor, 0, len(rows))
	for _, row := range rows {
		descriptors = append(descriptors, descriptor{row.key, row.digest, row.proof, row.expires})
	}
	view := selection.Digest(descriptors)
	if prior != nil && !watch && prior.View != view {
		return selection.Response{}, errSelectedChanged
	}
	expires := minSelectedTime(time.Now().Add(selectedCursorLifetime), a.identity.NotAfter).UnixMilli()
	if prior != nil && prior.View == view {
		expires = prior.Expires
	}
	cursor := selectedCursor{Version: 1, Purpose: "view", Tenant: n.cfg.Tenant, Site: n.cfg.Site, Principal: a.identity.Principal, Key: a.identity.SigningKeySHA256, Grant: a.identity.GrantID, Selection: selection.Digest(request.Selection), View: view, Expires: expires}
	revision, err := n.encodeSelected(cursor)
	if err != nil {
		return selection.Response{}, err
	}
	out := selection.Response{ReplicaScope: n.store.Subscription().Scope(), Version: 1, Components: []world.Component{}, Revision: revision, Complete: true, ObservedAt: cp.CapturedAt, TimedOut: watch}
	start := 0
	if prior != nil && !watch {
		for start < len(rows) && rows[start].key <= prior.After {
			start++
		}
		if start == 0 || rows[start-1].key != prior.After {
			return out, errSelectedCursor
		}
	}
	for index := start; index < len(rows) && len(out.Components) < request.PageSize; index++ {
		previous := out
		out.Components = append(out.Components, rows[index].component)
		if request.Selection.Region != nil {
			d := request.Selection.Region.Classify(rows[index].component)
			d.ExpiresAt = minSelectedTime(rows[index].expires, rows[index].authorityUntil)
			out.Spatial = append(out.Spatial, d)
		}
		out.Complete = index == len(rows)-1
		out.NextCursor = ""
		if !out.Complete {
			next := cursor
			next.Purpose = "page"
			next.After = rows[index].key
			out.NextCursor, err = n.encodeSelected(next)
			if err != nil {
				return selection.Response{}, err
			}
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return selection.Response{}, err
		}
		if len(raw)+1 > request.MaxBytes {
			if len(previous.Components) == 0 {
				return selection.Response{}, errSelectedOversize
			}
			out = previous
			break
		}
	}
	// Reserve space for the optional watch timeout flag throughout page
	// assembly. The handler decides whether the flag is needed.
	if watch {
		out.TimedOut = true
	}
	raw, err := json.Marshal(out)
	if err != nil || len(raw)+1 > request.MaxBytes {
		return selection.Response{}, errSelectedOversize
	}
	out.TimedOut = false
	if err = n.selectedGrant(ctx, a); err != nil {
		return selection.Response{}, err
	}
	if err = ctx.Err(); err != nil {
		return selection.Response{}, err
	}
	if !time.Now().Before(earliest) {
		return selection.Response{}, errSelectedChanged
	}
	if time.Now().UnixMilli() >= expires {
		return selection.Response{}, errSelectedExpired
	}
	return out, nil
}
