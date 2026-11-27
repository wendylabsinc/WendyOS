package node

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/internal/zonepub"
)

const RepairVersion = 1
const SelectiveRepairVersion = 2
const MaxRepairInventoryEntries = 64
const MaxRepairInventoryBytes = 128 << 10
const MaxRepairPageBytes = 512 << 10
const maxRepairCursorBytes = 2048
const repairCursorLifetime = 2 * time.Minute
const maxRepairPeers = 32

var errRepairBusy = errors.New("bulk repair is busy")

// Only bounded transfer hints survive a deferred round. Nothing here grants
// authority, extends a lifetime, or survives a process restart.
type repairProgress struct {
	Expires     time.Time
	Cursor      string
	Last        string
	Seen        int
	Pending     []RepairEntry
	Offset      int
	SingleUntil int
	HadDenied   bool
	NextCursor  string
	PageReady   bool
}

type repairHTTPError struct{ Status int }

func (e repairHTTPError) Error() string { return fmt.Sprintf("repair peer returned %d", e.Status) }

type repairProtocolError struct{ cause error }

func (e repairProtocolError) Error() string { return "invalid repair response: " + e.cause.Error() }
func (e repairProtocolError) Unwrap() error { return e.cause }

// RepairEntry is a transfer hint, never proof of current authority or visibility.
// ExpiresAt is the retained cap, not a new TTL at receipt. ProofSHA256 covers
// the complete origin envelope except its unsigned age hint.
type RepairEntry struct {
	Key             world.Key     `json:"key"`
	Version         world.Version `json:"version"`
	ComponentSHA256 string        `json:"component_sha256"`
	ProofSHA256     string        `json:"proof_sha256"`
	ExpiresAt       time.Time     `json:"expires_at"`
}
type RepairInventoryRequest struct {
	Subscription *subscription.Policy `json:"subscription,omitempty"`
	Version      int                  `json:"version"`
	Cursor       string               `json:"cursor,omitempty"`
}
type RepairInventoryResponse struct {
	SubscriptionSHA256 string               `json:"subscription_sha256,omitempty"`
	SourceSubscription *subscription.Policy `json:"source_subscription,omitempty"`
	Version            int                  `json:"version"`
	Entries            []RepairEntry        `json:"entries"`
	Cursor             string               `json:"cursor,omitempty"`
}
type RepairFetchRequest struct {
	Subscription *subscription.Policy `json:"subscription,omitempty"`
	Version      int                  `json:"version"`
	Entries      []RepairEntry        `json:"entries"`
}
type RepairRecord struct {
	Entry  RepairEntry            `json:"entry"`
	Record authority.SignedRecord `json:"record"`
}
type RepairFetchResponse struct {
	SubscriptionSHA256 string         `json:"subscription_sha256,omitempty"`
	Version            int            `json:"version"`
	Records            []RepairRecord `json:"records"`
	// Unavailable contains exact requested key hashes that changed, lost
	// authority, or did not fit the response. A later round may request them.
	Unavailable []string `json:"unavailable"`
}
type repairCursor struct {
	SubscriptionSHA256 string `json:"subscription_sha256,omitempty"`
	Version            int    `json:"version"`
	Tenant             string `json:"tenant"`
	Site               string `json:"site"`
	Principal          string `json:"principal"`
	KeyID              string `json:"key_id"`
	GrantID            string `json:"grant_id"`
	After              string `json:"after"`
	Expires            int64  `json:"expires_unix_ms"`
}

func repairKeyDigest(key world.Key) string {
	b, _ := json.Marshal(key)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func repairProofDigest(record authority.SignedRecord) (string, error) {
	record.Component.Timing.AgeHint = 0
	b, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func repairEntry(c world.CheckpointEntry, proof authority.SignedRecord) (RepairEntry, error) {
	d, err := recordDigest(c.Component)
	if err != nil {
		return RepairEntry{}, err
	}
	p, err := repairProofDigest(proof)
	if err != nil {
		return RepairEntry{}, err
	}
	return RepairEntry{Key: c.Component.Key, Version: c.Component.Version, ComponentSHA256: d, ProofSHA256: p, ExpiresAt: c.ExpiresAt.Round(0)}, nil
}
func sameRepairEntry(a, b RepairEntry) bool {
	return a.Key == b.Key && a.Version == b.Version && a.ComponentSHA256 == b.ComponentSHA256 && a.ProofSHA256 == b.ProofSHA256 && a.ExpiresAt.Equal(b.ExpiresAt)
}
func validRepairEntry(e RepairEntry, tenant, site string) bool {
	digest := func(s string) bool {
		b, err := hex.DecodeString(s)
		return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
	}
	return e.Key.Tenant == tenant && e.Key.Site == site && e.Key.Entity != "" && e.Key.Type != "" && e.Key.Source.Validate() == nil && e.Version.Epoch > 0 && e.Version.Sequence > 0 && digest(e.ComponentSHA256) && digest(e.ProofSHA256) && !e.ExpiresAt.IsZero()
}
func needsRepair(remote RepairEntry, local RepairEntry, found bool) bool {
	if !found {
		return true
	}
	if cmp := remote.Version.Compare(local.Version); cmp != 0 {
		return cmp > 0
	}
	if local.ProofSHA256 == "" && local.ExpiresAt.IsZero() {
		return remote.ComponentSHA256 != local.ComponentSHA256
	}
	return remote.ComponentSHA256 != local.ComponentSHA256 || remote.ProofSHA256 != local.ProofSHA256 || remote.ExpiresAt.Before(local.ExpiresAt)
}

func repairBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(b)) > limit {
		http.Error(w, "repair request too large or unreadable", http.StatusBadRequest)
		return nil, false
	}
	return b, true
}
func writeRepair(w http.ResponseWriter, value any, limit int) {
	b, err := json.Marshal(value)
	if err != nil || len(b)+1 > limit {
		http.Error(w, "repair response exceeds budget", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(b, '\n'))
}
func (n *Node) encodeRepairCursor(c repairCursor) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, n.repairKey[:])
	_, _ = mac.Write(b)
	token := base64.RawURLEncoding.EncodeToString(append(b, mac.Sum(nil)...))
	if len(token) > maxRepairCursorBytes {
		return "", errors.New("repair cursor exceeds limit")
	}
	return token, nil
}
func (n *Node) decodeRepairCursor(token string, id authority.Identity) (repairCursor, error) {
	if len(token) > maxRepairCursorBytes {
		return repairCursor{}, errors.New("repair cursor too large")
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) <= sha256.Size || base64.RawURLEncoding.EncodeToString(b) != token {
		return repairCursor{}, errors.New("invalid repair cursor")
	}
	payload, sig := b[:len(b)-sha256.Size], b[len(b)-sha256.Size:]
	mac := hmac.New(sha256.New, n.repairKey[:])
	_, _ = mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return repairCursor{}, errors.New("invalid repair cursor signature")
	}
	var c repairCursor
	if err := authority.DecodeStrict(payload, &c); err != nil {
		return c, err
	}
	now := time.Now().UnixMilli()
	if (c.Version != RepairVersion && c.Version != SelectiveRepairVersion) || (c.Version == RepairVersion && c.SubscriptionSHA256 != "") || (c.Version == SelectiveRepairVersion && !selectedHash(c.SubscriptionSHA256)) || c.Tenant != n.cfg.Tenant || c.Site != n.cfg.Site || c.Principal != id.Principal || c.KeyID != id.SigningKeySHA256 || c.GrantID != id.GrantID || c.Expires <= now || c.Expires > time.Now().Add(repairCursorLifetime).UnixMilli() || len(c.After) != 64 {
		return c, errors.New("stale or wrong-scope repair cursor")
	}
	a, err := hex.DecodeString(c.After)
	if err != nil || hex.EncodeToString(a) != c.After {
		return c, errors.New("invalid repair continuation")
	}
	return c, nil
}

func (n *Node) repairInventory(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) || !n.admitRepair(w, r) {
		return
	}
	defer n.repairIn.Unlock()
	body, ok := repairBody(w, r, subscription.MaxBytes)
	if !ok {
		return
	}
	id, ok := n.authorize(w, r, "replicate", body)
	if !ok {
		return
	}
	var request RepairInventoryRequest
	if authority.DecodeStrict(body, &request) != nil || n.validateRepairSubscription(request.Version, request.Subscription) != nil {
		http.Error(w, "invalid inventory request", 400)
		return
	}
	cursor := repairCursor{SubscriptionSHA256: request.Subscription.Digest(), Version: request.Version, Tenant: n.cfg.Tenant, Site: n.cfg.Site, Principal: id.Principal, KeyID: id.SigningKeySHA256, GrantID: id.GrantID, Expires: time.Now().Add(repairCursorLifetime).UnixMilli()}
	if request.Cursor != "" {
		var err error
		cursor, err = n.decodeRepairCursor(request.Cursor, id)
		if err != nil || cursor.Version != request.Version || cursor.SubscriptionSHA256 != request.Subscription.Digest() {
			if err == nil {
				err = errors.New("repair cursor subscription changed")
			}
			http.Error(w, err.Error(), 400)
			return
		}
	}
	response, err := n.inventoryPage(cursor, request.Subscription)
	if err != nil {
		http.Error(w, "inventory unavailable", 503)
		return
	}
	writeRepair(w, response, MaxRepairInventoryBytes)
}
func (n *Node) inventoryPage(cursor repairCursor, filter *subscription.Policy) (RepairInventoryResponse, error) {
	n.lock()
	defer n.mu.Unlock()
	cp, err := n.store.RepairSnapshot()
	if err != nil {
		return RepairInventoryResponse{}, err
	}
	// Ordering uses bounded key hashes. Only this page's proofs are hashed;
	// the existing store checkpoint still serializes its bounded whole state.
	sort.Slice(cp.Entries, func(i, j int) bool {
		return repairKeyDigest(cp.Entries[i].Component.Key) < repairKeyDigest(cp.Entries[j].Component.Key)
	})
	out := RepairInventoryResponse{SubscriptionSHA256: filter.Digest(), SourceSubscription: n.store.Subscription(), Version: cursor.Version, Entries: []RepairEntry{}}
	last := cursor.After
	for _, c := range cp.Entries {
		if !filter.Allows(c.Component.Key) {
			continue
		}
		key := repairKeyDigest(c.Component.Key)
		if key <= cursor.After {
			continue
		}
		if len(out.Entries) >= MaxRepairInventoryEntries {
			cursor.After = last
			out.Cursor, err = n.encodeRepairCursor(cursor)
			return out, err
		}
		d, err := recordDigest(c.Component)
		if err != nil {
			return out, err
		}
		proof, ok := n.proofs[d]
		if !ok {
			return out, errors.New("missing repair origin proof")
		}
		entry, err := repairEntry(c, proof)
		if err != nil {
			return out, err
		}
		candidate := out
		candidate.Entries = append(append([]RepairEntry(nil), out.Entries...), entry)
		b, err := json.Marshal(candidate)
		if err != nil {
			return out, err
		}
		if len(b)+maxRepairCursorBytes+64 > MaxRepairInventoryBytes {
			if len(out.Entries) == 0 {
				return out, errors.New("inventory entry exceeds page budget")
			}
			cursor.After = last
			out.Cursor, err = n.encodeRepairCursor(cursor)
			return out, err
		}
		out = candidate
		last = key
	}
	return out, nil
}

func (n *Node) repairFetch(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) || !n.admitRepair(w, r) {
		return
	}
	defer n.repairIn.Unlock()
	body, ok := repairBody(w, r, MaxRepairPageBytes)
	if !ok {
		return
	}
	if _, ok = n.authorize(w, r, "replicate", body); !ok {
		return
	}
	var request RepairFetchRequest
	if authority.DecodeStrictLimit(body, &request, MaxRepairPageBytes) != nil || n.validateRepairSubscription(request.Version, request.Subscription) != nil || len(request.Entries) == 0 || len(request.Entries) > store.MaxRepairRecords {
		http.Error(w, "invalid repair fetch", 400)
		return
	}
	seen := map[string]bool{}
	for _, e := range request.Entries {
		k := repairKeyDigest(e.Key)
		if !validRepairEntry(e, n.cfg.Tenant, n.cfg.Site) || !request.Subscription.Allows(e.Key) || seen[k] {
			http.Error(w, "invalid or repeated repair key", 400)
			return
		}
		seen[k] = true
	}
	records, missing, err := n.captureRepairRecords(request.Entries)
	if err != nil {
		http.Error(w, "repair records unavailable", 503)
		return
	}
	out := RepairFetchResponse{SubscriptionSHA256: request.Subscription.Digest(), Version: request.Version, Records: []RepairRecord{}, Unavailable: missing}
	if len(records) > 0 {
		proofs := make([]authority.SignedRecord, len(records))
		for i := range records {
			proofs[i] = records[i].Record
		}
		if _, err = n.verifyRepairRecordsContext(r.Context(), proofs); err != nil {
			if !errors.Is(err, authority.ErrDenied) {
				http.Error(w, "repair authority unavailable", 503)
				return
			}
			// A revoked source must not permanently block an unrelated source
			// in the same transfer page. The fallback is bounded to eight checks.
			valid := records[:0]
			for _, record := range records {
				if _, e := n.verifyRepairRecordsContext(r.Context(), []authority.SignedRecord{record.Record}); e == nil {
					valid = append(valid, record)
				} else if errors.Is(e, authority.ErrDenied) {
					out.Unavailable = append(out.Unavailable, repairKeyDigest(record.Entry.Key))
				} else {
					http.Error(w, "repair authority unavailable", 503)
					return
				}
			}
			records = valid
		}
	}
	for _, record := range records {
		out.Records = append(out.Records, record)
		b, err := json.Marshal(out)
		// Leave room for one 64-byte unavailable key per remaining record.
		if err != nil || len(b)+store.MaxRepairRecords*70+1 > MaxRepairPageBytes {
			out.Records = out.Records[:len(out.Records)-1]
			out.Unavailable = append(out.Unavailable, repairKeyDigest(record.Entry.Key))
		}
	}
	writeRepair(w, out, MaxRepairPageBytes)
}
func (n *Node) captureRepairRecords(wanted []RepairEntry) ([]RepairRecord, []string, error) {
	n.lock()
	defer n.mu.Unlock()
	cp, err := n.store.RepairSnapshot()
	if err != nil {
		return nil, nil, err
	}
	entries := map[world.Key]world.CheckpointEntry{}
	for _, c := range cp.Entries {
		entries[c.Component.Key] = c
	}
	result := []RepairRecord{}
	missing := []string{}
	for _, want := range wanted {
		key := repairKeyDigest(want.Key)
		c, ok := entries[want.Key]
		if !ok {
			missing = append(missing, key)
			continue
		}
		d, err := recordDigest(c.Component)
		if err != nil {
			return nil, nil, err
		}
		p, ok := n.proofs[d]
		if !ok {
			return nil, nil, errors.New("missing captured origin proof")
		}
		entry, err := repairEntry(c, p)
		if err != nil {
			return nil, nil, err
		}
		if !sameRepairEntry(entry, want) {
			missing = append(missing, key)
			continue
		}
		// Copy all signed bytes under the lock; the later verifier never
		// resolves a proof again against a concurrently changing replica.
		b, err := json.Marshal(p)
		if err != nil {
			return nil, nil, err
		}
		var copied authority.SignedRecord
		if err = authority.DecodeStrict(b, &copied); err != nil {
			return nil, nil, err
		}
		result = append(result, RepairRecord{Entry: entry, Record: copied})
	}
	return result, missing, nil
}
func (n *Node) verifyRepairRecords(records []authority.SignedRecord) ([]authority.Identity, error) {
	return n.verifyRepairRecordsContext(context.Background(), records)
}
func (n *Node) verifyRepairRecordsContext(ctx context.Context, records []authority.SignedRecord) ([]authority.Identity, error) {
	ids, err := n.verifier.VerifyRecordsContext(ctx, records)
	if err != nil {
		return nil, err
	}
	for i, record := range records {
		if record.Component.Key.Type == zonepub.Type {
			if ids[i].Kind != "operator" || !ids[i].Allows("site-publish") {
				return nil, authority.ErrDenied
			}
			if _, err := zonepub.Validate(record.Component); err != nil {
				return nil, err
			}
		}
		if record.Component.Key.Type == sitepub.Type {
			if ids[i].Kind != "operator" || !ids[i].Allows("site-publish") {
				return nil, authority.ErrDenied
			}
			p, err := sitepub.Validate(record.Component)
			now := time.Now()
			if err != nil || now.Before(p.ValidFrom) || !now.Before(p.ValidUntil) {
				return nil, authority.ErrDenied
			}
		}
	}
	return ids, nil
}

func (n *Node) applyRepairPage(records []RepairRecord) ([]world.Result, error) {
	return n.applyRepairPageContext(context.Background(), records)
}
func (n *Node) applyRepairPageContext(ctx context.Context, records []RepairRecord) ([]world.Result, error) {
	if len(records) == 0 || len(records) > store.MaxRepairRecords {
		return nil, repairProtocolError{errors.New("invalid repair page size")}
	}
	b, err := json.Marshal(records)
	if err != nil || len(b) > MaxRepairPageBytes {
		return nil, repairProtocolError{errors.New("repair page exceeds budget")}
	}
	// A decoded private copy prevents callers from changing candidates after
	// signature checks or while the proof callback blocks.
	var copied []RepairRecord
	if err = authority.DecodeStrictLimit(b, &copied, MaxRepairPageBytes); err != nil {
		return nil, repairProtocolError{err}
	}
	proofs := make([]authority.SignedRecord, len(copied))
	seen := map[world.Key]bool{}
	for i, r := range copied {
		if !validRepairEntry(r.Entry, n.cfg.Tenant, n.cfg.Site) || seen[r.Entry.Key] || !n.store.AcceptsKey(r.Entry.Key) {
			return nil, repairProtocolError{errors.New("invalid repeated repair entry")}
		}
		seen[r.Entry.Key] = true
		d, err := recordDigest(r.Record.Component)
		if err != nil {
			return nil, repairProtocolError{err}
		}
		p, err := repairProofDigest(r.Record)
		if err != nil {
			return nil, repairProtocolError{err}
		}
		if r.Entry.Key != r.Record.Component.Key || r.Entry.Version != r.Record.Component.Version || d != r.Entry.ComponentSHA256 || p != r.Entry.ProofSHA256 {
			return nil, repairProtocolError{errors.New("repair origin digest mismatch")}
		}
		proofs[i] = r.Record
	}
	ids, err := n.verifyRepairRecordsContext(ctx, proofs)
	if err != nil {
		return nil, err
	}
	page := make([]store.Publication, len(copied))
	for i, r := range copied {
		page[i] = store.Publication{Source: ids[i].Source, Component: r.Record.Component, ExpiresAt: r.Entry.ExpiresAt}
	}
	n.lock()
	defer n.mu.Unlock()
	// Different valid envelopes can attest to the same component. Prefer a
	// deterministic current proof, so reciprocal pulls cannot swap envelopes
	// forever. This bounded conflict path keeps the existing node->verifier
	// lock order and runs before the Store callback. Node revocation installs
	// take n.mu too. The choice retains evidence; it grants no read authority.
	selected := make(map[string]authority.SignedRecord, len(copied))
	checkedLocal := false
	hasAmendment := false
	for _, incoming := range copied {
		hasAmendment = hasAmendment || annotations.IsAmendment(incoming.Record.Component)
		chosen := incoming.Record
		if prior, ok := n.proofs[incoming.Entry.ComponentSHA256]; ok {
			priorDigest, err := repairProofDigest(prior)
			if err != nil {
				return nil, err
			}
			if priorDigest != incoming.Entry.ProofSHA256 && (prior.SignedAtUnixMS > incoming.Record.SignedAtUnixMS || (prior.SignedAtUnixMS == incoming.Record.SignedAtUnixMS && priorDigest > incoming.Entry.ProofSHA256)) {
				checkedLocal = true
				priorIDs, err := n.verifyRepairRecordsContext(ctx, []authority.SignedRecord{prior})
				if err == nil {
					_, err = n.regionalCurrent(ctx, prior, priorIDs[0])
				}
				if err == nil {
					chosen = prior
				} else if !errors.Is(err, authority.ErrDenied) {
					return nil, err
				}
			}
		}
		selected[incoming.Entry.ComponentSHA256] = chosen
	}
	if checkedLocal || hasAmendment || n.store.Subscription().Regional() {
		current := make([]authority.SignedRecord, len(copied))
		for i, incoming := range copied {
			current[i] = selected[incoming.Entry.ComponentSHA256]
		}
		// Local proof checks can block too. Verify the finally selected group
		// after all of them, before entering the persistence callback.
		// Amendments always require this pass: the first verification happened
		// before n.mu, while a node revocation could still be installed.
		var checkedErr error
		ids, checkedErr = n.verifyRepairRecordsContext(ctx, current)
		if checkedErr != nil {
			return nil, checkedErr
		}
		for i, r := range current {
			ids[i], checkedErr = n.regionalCurrent(ctx, r, ids[i])
			if checkedErr != nil {
				return nil, checkedErr
			}
		}
	}
	var amendmentUntil time.Time
	for i, r := range copied {
		if (annotations.IsAmendment(r.Record.Component) || n.store.Subscription().Regional()) && (amendmentUntil.IsZero() || ids[i].NotAfter.Before(amendmentUntil)) {
			amendmentUntil = ids[i].NotAfter
		}
	}
	results, err := n.store.ApplyPreparedBatch(page, func(state store.BatchPublicationState) error {
		if !amendmentUntil.IsZero() && !time.Now().Before(amendmentUntil) {
			return authority.ErrDenied
		}
		coverage := map[string]authority.SignedRecord{}
		for _, c := range state.Records {
			d, err := recordDigest(c)
			if err != nil {
				return err
			}
			p, ok := n.proofs[d]
			if !ok {
				return errors.New("missing current repair proof")
			}
			coverage[d] = p
		}
		annotationCandidates := append([]world.Component(nil), state.Records...)
		for _, r := range copied {
			if annotations.Reserved(r.Record.Component) {
				for _, old := range annotationCandidates {
					if annotationIdentity(old.Key) == annotationIdentity(r.Entry.Key) {
						d, err := recordDigest(old)
						if err != nil {
							return err
						}
						if d != r.Entry.ComponentSHA256 {
							return ErrAnnotationConflict
						}
					}
				}
			}
			annotationCandidates = append(annotationCandidates, r.Record.Component)
			if n.store.Retains(r.Record.Component) {
				coverage[r.Entry.ComponentSHA256] = selected[r.Entry.ComponentSHA256]
			}
		}
		if len(coverage) > n.cfg.MaxRecords+store.MaxRepairRecords {
			return errors.New("repair proof capacity reached")
		}
		if err := n.saveProofs(coverage); err != nil {
			return err
		}
		n.proofs = coverage
		if !amendmentUntil.IsZero() && !time.Now().Before(amendmentUntil) {
			return authority.ErrDenied
		}
		return nil
	})
	if err == nil {
		err = n.pruneRegionalProofs()
	}
	return results, err
}

func (n *Node) localRepairInventory() (map[world.Key]RepairEntry, error) {
	n.lock()
	defer n.mu.Unlock()
	cp, err := n.store.RepairSnapshot()
	if err != nil {
		return nil, err
	}
	out := make(map[world.Key]RepairEntry, len(cp.Entries))
	for _, c := range cp.Entries {
		d, err := recordDigest(c.Component)
		if err != nil {
			return nil, err
		}
		p, ok := n.proofs[d]
		if !ok {
			return nil, errors.New("missing local repair proof")
		}
		e, err := repairEntry(c, p)
		if err != nil {
			return nil, err
		}
		out[e.Key] = e
	}
	for _, f := range cp.Fences {
		out[f.Key] = RepairEntry{Key: f.Key, Version: f.Version, ComponentSHA256: f.SHA256}
	}
	return out, nil
}
func (n *Node) repairRequest(ctx context.Context, peer Peer, credentials Credentials, path string, request, response any, limit int) error {
	endpoint, err := url.Parse(peer.URL)
	if err != nil || endpoint.Scheme != "http" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("invalid simulation peer URL")
	}
	endpoint.Path = path
	endpoint.RawPath = ""
	body, err := json.Marshal(request)
	if err != nil || len(body) > MaxRepairPageBytes {
		return errors.New("repair request exceeds budget")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	if err = AuthorizeRequest(req, credentials, peer.Audience, "replicate", body); err != nil {
		return err
	}
	reply, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer reply.Body.Close()
	if reply.StatusCode != http.StatusOK {
		return repairHTTPError{Status: reply.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(reply.Body, int64(limit+1)))
	if err != nil {
		return err
	}
	if err := authority.DecodeStrictLimit(b, response, limit); err != nil {
		return repairProtocolError{cause: err}
	}
	return nil
}
func repairPeerKey(peer Peer, credentials Credentials) (string, error) {
	encoded, err := json.Marshal(struct {
		Peer  Peer
		Grant authority.SignedGrant
	}{peer, credentials.Grant})
	if err != nil || len(encoded) > authority.MaxEnvelopeBytes {
		return "", errors.New("repair peer identity exceeds budget")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (n *Node) repairRound(ctx context.Context, peer Peer, credentials Credentials) (resultErr error) {
	ctx, traceRequest := n.cfg.Trace.Start(ctx, diagnostics.RepairRound)
	defer traceRequest.Finish(0)
	if !n.repairOut.TryLock() {
		return errRepairBusy
	}
	defer n.repairOut.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	key, err := repairPeerKey(peer, credentials)
	if err != nil {
		return err
	}
	var policy *subscription.Policy
	if n.store != nil {
		policy = n.store.Subscription()
	}
	protocol := RepairVersion
	if policy != nil {
		protocol = SelectiveRepairVersion
	}
	n.noteReplicaAttempt(key, peer)
	defer func() { n.noteReplicaResult(key, resultErr) }()
	now := time.Now()
	if n.repairProgress == nil {
		n.repairProgress = map[string]*repairProgress{}
	}
	for k, p := range n.repairProgress {
		if !now.Before(p.Expires) {
			delete(n.repairProgress, k)
		}
	}
	progress, ok := n.repairProgress[key]
	if !ok {
		if len(n.repairProgress) >= maxRepairPeers {
			return errRepairBusy
		}
		progress = &repairProgress{Expires: now.Add(repairCursorLifetime)}
		n.repairProgress[key] = progress
	}
	defer func() {
		var status repairHTTPError
		var protocol repairProtocolError
		if (errors.As(resultErr, &status) && status.Status == http.StatusBadRequest) || errors.As(resultErr, &protocol) {
			delete(n.repairProgress, key)
		}
	}()
	discard := func(reason error) error { delete(n.repairProgress, key); return reason }
	var local map[world.Key]RepairEntry
	for {
		if err := n.waitRepairTurn(ctx); err != nil {
			return err
		}
		if !time.Now().Before(progress.Expires) {
			return discard(errors.New("repair continuation expired"))
		}
		if !progress.PageReady {
			var inventory RepairInventoryResponse
			if err = n.repairRequest(ctx, peer, credentials, "/v1/repair/inventory", RepairInventoryRequest{Subscription: policy, Version: protocol, Cursor: progress.Cursor}, &inventory, MaxRepairInventoryBytes); err != nil {
				return err
			}
			if inventory.Version != protocol || inventory.SubscriptionSHA256 != policy.Digest() || n.validateSourceSubscription(inventory.SourceSubscription) != nil || len(inventory.Entries) > MaxRepairInventoryEntries || len(inventory.Cursor) > maxRepairCursorBytes || (inventory.Cursor != "" && (inventory.Cursor == progress.Cursor || len(inventory.Entries) == 0)) {
				return discard(errors.New("invalid repair inventory page"))
			}
			if policy.Regional() && inventory.SourceSubscription != nil {
				return discard(errors.New("regional storage repair requires a full source; partial peers cannot supply complete movement fences"))
			}
			n.noteReplicaSource(key, inventory.SourceSubscription)
			// Validate the whole page before retaining any new traversal state.
			last, seen := progress.Last, progress.Seen
			for _, e := range inventory.Entries {
				order := repairKeyDigest(e.Key)
				seen++
				if !validRepairEntry(e, n.cfg.Tenant, n.cfg.Site) || !policy.Allows(e.Key) || order <= last || seen > n.cfg.MaxRecords {
					return discard(errors.New("invalid or repeated repair inventory entry"))
				}
				last = order
			}
			progress.Last, progress.Seen = last, seen
			progress.Pending = inventory.Entries
			progress.Offset = 0
			progress.SingleUntil = 0
			progress.NextCursor = inventory.Cursor
			progress.PageReady = true
		}
		if progress.Offset < len(progress.Pending) {
			if err := n.waitRepairTurn(ctx); err != nil {
				return err
			}
			if local == nil {
				local, err = n.localRepairInventory()
				if err != nil {
					return err
				}
			}
			end := progress.Offset + store.MaxRepairRecords
			if progress.Offset < progress.SingleUntil {
				end = progress.Offset + 1
			}
			if end > len(progress.Pending) {
				end = len(progress.Pending)
			}
			wanted := []RepairEntry{}
			for _, entry := range progress.Pending[progress.Offset:end] {
				prior, found := local[entry.Key]
				if needsRepair(entry, prior, found) {
					wanted = append(wanted, entry)
				}
			}
			if len(wanted) > 0 {
				request := RepairFetchRequest{Subscription: policy, Version: protocol, Entries: wanted}
				var response RepairFetchResponse
				if err = n.repairRequest(ctx, peer, credentials, "/v1/repair/fetch", request, &response, MaxRepairPageBytes); err != nil {
					return err
				}
				n.cfg.Metrics.Add(runtimeobs.RepairRecordsFetched, uint64(len(response.Records)))
				if err = validateRepairFetch(request, response); err != nil {
					n.cfg.Metrics.Increment(runtimeobs.RepairRejected)
					return discard(err)
				}
				if len(response.Records) > 0 {
					if err := n.waitRepairTurn(ctx); err != nil {
						return err
					}
					results, err := n.applyRepairPageContext(ctx, response.Records)
					if err != nil {
						n.cfg.Metrics.Increment(runtimeobs.RepairRejected)
						if !errors.Is(err, authority.ErrDenied) {
							return err
						}
						progress.HadDenied = true
						// A denied batch admits no world records. Retry its bounded
						// segment as separately requested singleton pages so one
						// invalid origin cannot block authorized neighbours. Each
						// singleton receives fresh origin checks and an atomic write.
						// Verifier denial-floor rules remain unchanged; malformed or
						// revoked identities do not gain a ledger write here.
						if len(response.Records) > 1 {
							progress.SingleUntil = end
							continue
						}
					}
					for _, result := range results {
						switch result {
						case world.Applied:
							n.cfg.Metrics.Increment(runtimeobs.RepairApplied)
						case world.Duplicate:
							n.cfg.Metrics.Increment(runtimeobs.RepairDuplicate)
						case world.Obsolete:
							n.cfg.Metrics.Increment(runtimeobs.RepairObsolete)
						}
					}
				}
			}
			// Successful application, already satisfied/unavailable hints, or a
			// denied singleton advance this chunk. Interrupted fetches and other
			// commit failures retry exact metadata without granting authority.
			progress.Offset = end
			continue
		}
		if progress.NextCursor == "" {
			delete(n.repairProgress, key)
			if progress.HadDenied {
				return authority.ErrDenied
			}
			return nil
		}
		progress.Cursor = progress.NextCursor
		progress.NextCursor = ""
		progress.Pending = nil
		progress.Offset = 0
		progress.SingleUntil = 0
		progress.PageReady = false
	}
}
func validateRepairFetch(request RepairFetchRequest, response RepairFetchResponse) error {
	if response.Version != request.Version || response.SubscriptionSHA256 != request.Subscription.Digest() || len(response.Records)+len(response.Unavailable) != len(request.Entries) {
		return errors.New("invalid repair fetch coverage")
	}
	wanted := map[string]RepairEntry{}
	for _, e := range request.Entries {
		wanted[repairKeyDigest(e.Key)] = e
	}
	for _, r := range response.Records {
		key := repairKeyDigest(r.Entry.Key)
		e, ok := wanted[key]
		if !ok || !sameRepairEntry(e, r.Entry) {
			return errors.New("unsolicited or changed repair record")
		}
		delete(wanted, key)
		b, err := json.Marshal(r.Record)
		if err != nil || len(b) > authority.MaxEnvelopeBytes {
			return errors.New("repair record exceeds envelope limit")
		}
	}
	for _, key := range response.Unavailable {
		if _, ok := wanted[key]; !ok {
			return errors.New("invalid unavailable repair key")
		}
		delete(wanted, key)
	}
	if len(wanted) != 0 {
		return errors.New("incomplete repair response")
	}
	return nil
}
