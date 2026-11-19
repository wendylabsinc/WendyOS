package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// The device's half of the deployment contract (service-protos #185, the
// rules on DeviceAssignment and UnassignDeviceRequest in registry.proto).
// Statements for one deployment are ordered by revision, never by time.

const (
	// maxRevision is carried only by a terminal removal: the deployment is
	// then permanently dead on this device.
	maxRevision = math.MaxInt32
	// removalMarkKeep is how long a removal mark is kept: past the platform
	// cap (1 year), so no statement it guards can still be valid.
	removalMarkKeep = 400 * 24 * time.Hour
	// maxConfigSize caps DeviceAssignment.config_json.
	maxConfigSize = 64 << 10
)

// postboxMark is, per deployment, the highest revision this device accepted
// and whether that was a removal. RemovedAt (trusted unix seconds) dates a
// removal for pruning.
type postboxMark struct {
	Revision  int32 `json:"revision"`
	Removed   bool  `json:"removed,omitempty"`
	RemovedAt int64 `json:"removedAt,omitempty"`
}

// contractRefusal is a statement the contract refuses: nothing is applied
// and nothing held changes. old marks a statement that is not news (a copy
// behind the accepted revision, or a conflicting copy of it): audit only, so
// the held deployment keeps its reported outcome. A refusal of a newer
// revision is reported against the app: it answers what the operator just
// did, and the report's postbox_entry_id names the refused statement.
type contractRefusal struct {
	outcome cloudpbv2.ApplyOutcome
	reason  string
	err     error
	old     bool
}

func (r *contractRefusal) Error() string { return r.err.Error() }

func stale(reason, format string, args ...any) error {
	return &contractRefusal{outcome: cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE, reason: reason, err: fmt.Errorf(format, args...)}
}

// outOfOrder is a stale refusal of a statement that is not news.
func outOfOrder(reason, format string, args ...any) error {
	return &contractRefusal{outcome: cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE, reason: reason, err: fmt.Errorf(format, args...), old: true}
}

func notForThisDevice(reason, format string, args ...any) error {
	return &contractRefusal{outcome: cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_TARGET, reason: reason, err: fmt.Errorf(format, args...)}
}

// checkConfig is the device's check of a signed configuration: non-empty,
// exactly one JSON object, at most 64 KiB.
func checkConfig(config string) error {
	if config == "" {
		return errors.New("the configuration is empty")
	}
	if len(config) > maxConfigSize {
		return fmt.Errorf("the configuration is %d bytes, over the %d byte limit", len(config), maxConfigSize)
	}
	dec := json.NewDecoder(strings.NewReader(config))
	dec.UseNumber() // a number's text is kept: 1e400 is valid JSON
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("the configuration is not a JSON object")
	}
	if err := checkJSONObject(dec); err != nil {
		return fmt.Errorf("the configuration is not valid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("the configuration is more than one JSON object")
	}
	// Named values only: host access comes from the release, never from a
	// deployment (sem, 2026-11-12).
	_, err := configEnv(config)
	return err
}

// checkJSONObject reads the rest of an object whose '{' was read, refusing a
// key that appears twice in it or in any object nested inside it: which of
// two values counts would depend on the parser. Keys equal ignoring case
// count as the same key, because Go's JSON decoder (the start step's) matches
// field names without regard to case, so the last of them would win.
func checkJSONObject(dec *json.Decoder) error {
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		folded := foldKey(key)
		if seen[folded] {
			return fmt.Errorf("key %q appears twice", key)
		}
		seen[folded] = true
		if err := checkJSONValue(dec); err != nil {
			return err
		}
	}
	_, err := dec.Token() // '}'
	return err
}

// foldKey maps every rune to the smallest rune of its case-fold orbit, so
// foldKey(a) == foldKey(b) exactly when strings.EqualFold(a, b).
func foldKey(s string) string {
	return strings.Map(func(r rune) rune {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		return least
	}, s)
}

func checkJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		return checkJSONObject(dec)
	case json.Delim('['):
		for dec.More() {
			if err := checkJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token() // ']'
	}
	return err
}

// assignOrder applies the ordering rules to an assignment against what the
// device holds. repeat is an assignment at the revision already accepted
// with identical content (a key rotation): it replaces the held statement
// and applies nothing again, replaces_* included. A refusal is a
// *contractRefusal.
func assignOrder(st *postboxState, a *cloudpbv2.DeviceAssignment) (repeat bool, err error) {
	dep, rev := a.GetDeploymentId(), a.GetRevision()
	if rev <= 0 || rev == maxRevision {
		return false, stale("revision", "assignment revision %d is not a deployment revision", rev)
	}
	if m := st.Marks[dep]; m != nil {
		switch {
		case m.Removed && rev <= m.Revision:
			return false, outOfOrder("revision", "deployment was removed at revision %d; assignment revision %d", m.Revision, rev)
		case rev < m.Revision:
			return false, outOfOrder("revision", "assignment revision %d is below the accepted revision %d", rev, m.Revision)
		case rev == m.Revision:
			held := st.Assignments[dep]
			prev := &cloudpbv2.DeviceAssignment{}
			if held == nil || proto.Unmarshal(held.Assignment, prev) != nil || !sameContent(prev, a) {
				return false, outOfOrder("revision_conflict", "assignment revision %d differs from the one already accepted", rev)
			}
			return true, nil
		}
	}
	if err := checkConfig(a.GetConfigJson()); err != nil {
		return false, &contractRefusal{outcome: cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, reason: "config", err: err}
	}
	from, fromRev := a.GetReplacesDeploymentId(), a.GetReplacesRevision()
	if from != "" || fromRev != 0 {
		switch {
		case from == "" || from == dep || fromRev <= 0 || fromRev == maxRevision:
			return false, notForThisDevice("move", "move from %q at revision %d is not a move", from, fromRev)
		case st.Assignments[from] != nil && st.Assignments[from].AppID != a.GetAppId():
			return false, notForThisDevice("move", "move from a deployment of app %q to app %q", st.Assignments[from].AppID, a.GetAppId())
		}
		if m := st.Marks[from]; m != nil && (fromRev < m.Revision || fromRev == m.Revision && !m.Removed) {
			return false, stale("move", "move from %q at revision %d; this device accepted revision %d of it", from, fromRev, m.Revision)
		}
	}
	for other, held := range st.Assignments {
		if other != dep && other != from && held.AppID == a.GetAppId() {
			return false, notForThisDevice("one_instance", "app %q already runs here in deployment %q", a.GetAppId(), other)
		}
	}
	return false, nil
}

// sameContent compares what an equal-revision assignment must repeat; the
// key, wrap and expiry may differ (rotation).
func sameContent(x, y *cloudpbv2.DeviceAssignment) bool {
	return x.GetAppId() == y.GetAppId() && x.GetAppReleaseId() == y.GetAppReleaseId() &&
		x.GetManifestDigest() == y.GetManifestDigest() && x.GetConfigJson() == y.GetConfigJson() &&
		x.GetDesiredState() == y.GetDesiredState() &&
		x.GetReplacesDeploymentId() == y.GetReplacesDeploymentId() && x.GetReplacesRevision() == y.GetReplacesRevision()
}

// unassignOrder applies the ordering rules to a removal. duplicate is one
// equal to the removal mark: consumed without effect.
func unassignOrder(st *postboxState, r *cloudpbv2.UnassignDeviceRequest) (duplicate bool, err error) {
	rev := r.GetRevision()
	if rev <= 0 {
		return false, stale("revision", "removal revision %d is not a deployment revision", rev)
	}
	m := st.Marks[r.GetDeploymentId()]
	switch {
	case m == nil || rev > m.Revision:
		return false, nil
	case m.Removed && rev == m.Revision:
		return true, nil
	}
	return false, stale("revision", "removal revision %d is not above the accepted revision %d", rev, m.Revision)
}

// markRemoved records a removal mark at now (trusted time) and drops removal
// marks older than removalMarkKeep. Pruning is measured against the
// persisted time floor, never the clock: a clock run forward must not drop a
// mark that still guards a valid statement.
func markRemoved(st *postboxState, dep string, rev int32, now time.Time) {
	for d, m := range st.Marks {
		if m.Removed && m.RemovedAt+int64(removalMarkKeep/time.Second) < st.TimeFloor {
			delete(st.Marks, d)
		}
	}
	st.Marks[dep] = &postboxMark{Revision: rev, Removed: true, RemovedAt: now.Unix()}
}

// refused turns a contract refusal into apply's result.
func refused(app string, err error) (string, cloudpbv2.ApplyOutcome, string, error) {
	var r *contractRefusal
	if errors.As(err, &r) {
		if r.old {
			app = "" // not news: audit only
		}
		return app, r.outcome, r.reason, r.err
	}
	return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "assignment", err
}

// applyAssignment holds a verified assignment the contract accepts. The
// state change is on disk before anything acts on it: the mark moves, the
// statement is held, a move's old deployment leaves with its removal mark,
// and an ABSENT revision queues the app's removal. Only then are pulls
// stopped or started and releases removed, so a crash in between never
// loses a change whose nonce is spent. A refused assignment changes nothing.
func (p *Postbox) applyAssignment(ctx context.Context, id *postboxIdentity, entryID int64, correlation string, r *cloudpbv2.DeviceAssignment) (string, cloudpbv2.ApplyOutcome, string, error) {
	app, dep := r.GetAppId(), r.GetDeploymentId()
	if outcome, reason, err := checkAssignment(r); err != nil {
		return app, outcome, reason, err
	}
	raw, err := proto.Marshal(r)
	if err != nil {
		return refused(app, err)
	}
	// Only this cycle changes marks and held statements; pulls flip flags.
	p.mu.Lock()
	repeat, err := assignOrder(p.state, r)
	p.mu.Unlock()
	if err != nil {
		return refused(app, err)
	}
	from := r.GetReplacesDeploymentId()
	move := from != "" && !repeat
	absent := r.GetDesiredState() == cloudpbv2.Running_ABSENT
	held := &postboxAssignment{
		AppID: app, AppReleaseID: r.GetAppReleaseId(), ManifestDigest: r.GetManifestDigest(),
		DeploymentKeyID: r.GetDeploymentKeyId(), DesiredState: r.GetDesiredState().String(),
		Assignment: raw, EntryID: entryID, CorrelationID: correlation, Pulled: absent,
		// An ABSENT revision is applied as it is held (its removal queued):
		// the last applied state, after which nothing is put back.
		Applied: absent,
	}
	replaced := dep
	if move {
		replaced = from
	}
	outcome := outcomePulling
	now := p.trustedNow()
	if err := p.commit(func(st *postboxState) {
		if repeat {
			// Key rotation: the new statement (wrap, key id, expiry) is held
			// and nothing is applied again. Only a pull that succeeded counts
			// as done; after a refused or failed one the copy is pulled again.
			prev, o := st.Assignments[dep], st.Outcomes[app]
			if prev.Pulled && o != nil && o.EntryID == prev.EntryID && pullSucceeded(o.Outcome) {
				held.Pulled, outcome = true, o.Outcome
			}
		}
		// The statement this one replaces on the device, started again if
		// this one cannot start; a rotation copy keeps the one it had.
		// The last applied statement, never a failed one; a rotation copy
		// keeps its predecessor's.
		switch prev := st.Assignments[replaced]; {
		case prev == nil:
		case repeat:
			held.Previous, held.Applied = prev.Previous, prev.Applied && held.Pulled
		case prev.Applied:
			held.Previous = prev.Assignment
		default:
			held.Previous = prev.Previous
		}
		if kept, ok := st.Detached[app]; ok {
			// A new deployment of a detached app replaces the direct run from
			// here on: removals act as usual, and if its start fails after
			// replacing the direct run, that app is put back as any last
			// applied statement is.
			if held.Previous == nil {
				held.Previous = kept
			}
			st.Detached = maps.Clone(st.Detached)
			delete(st.Detached, app)
		}
		st.Assignments = maps.Clone(st.Assignments) // copy on write: a pull's commit reads it
		st.Marks = maps.Clone(st.Marks)
		st.Assignments[dep] = held
		st.Marks[dep] = &postboxMark{Revision: r.GetRevision()}
		if move {
			delete(st.Assignments, from)
			markRemoved(st, from, r.GetReplacesRevision(), now)
		}
		if absent {
			queueRemoval(st, app)
		}
		markCommitted(st, entryID)
	}); err != nil {
		return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "save", err
	}
	// A newer entry for the deployment replaces its pull, and a move stops
	// the old deployment's; their outcomes no longer match what is held.
	p.stopPull(dep)
	if move {
		p.stopPull(from)
	}
	switch {
	case absent:
		// Held with nothing to fetch: the app's releases go, as on removal.
		if err := p.removeReleases(ctx, app); err != nil {
			return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "remove", err
		}
		return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED, "", nil
	case held.Pulled:
		return app, outcome, "", nil
	}
	// The pull runs beside the queue; its outcome replaces this one.
	p.startPull(ctx, id, r, entryID, correlation)
	return app, outcomePulling, "", nil
}

// pullSucceeded reports an ended pull's outcome that needs no new pull:
// applied, or imported and not started (sent as no outcome while the
// transient values are off).
func pullSucceeded(o cloudpbv2.ApplyOutcome) bool {
	switch o {
	case cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_NOT_STARTED, outcomeNotStarted:
		return true
	}
	return false
}

// markCommitted records that entryID's change is on disk, so an interrupted
// entry is finished on restart instead of reported failed.
func markCommitted(st *postboxState, entryID int64) {
	if st.Pending != nil && st.Pending.EntryID == entryID {
		pend := *st.Pending
		pend.Committed = true
		st.Pending = &pend
	}
}

// queueRemoval records app for release removal; removeReleases clears it once
// done, and every cycle retries it until then.
func queueRemoval(st *postboxState, app string) {
	if !slices.Contains(st.Removals, app) {
		st.Removals = append(slices.Clone(st.Removals), app)
	}
}

// applyUnassign removes a deployment the contract lets go. The removal is on
// disk first: the held statement goes, the mark becomes a removal mark and
// the app is queued for removal. Then its pull stops (so nothing it imports
// outlives the deployment) and the app's releases go; after a crash the
// queued removal is retried. A duplicate removal is consumed without effect.
func (p *Postbox) applyUnassign(ctx context.Context, entryID int64, r *cloudpbv2.UnassignDeviceRequest) (string, cloudpbv2.ApplyOutcome, string, error) {
	dep := r.GetDeploymentId()
	p.mu.Lock()
	duplicate, err := unassignOrder(p.state, r)
	p.mu.Unlock()
	if err != nil {
		return refused("", err)
	}
	if duplicate {
		return "", cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED, "", nil
	}
	now := p.trustedNow()
	app := ""
	if err := p.commit(func(st *postboxState) {
		if a := st.Assignments[dep]; a != nil {
			app = a.AppID
		}
		st.Assignments = maps.Clone(st.Assignments)
		st.Marks = maps.Clone(st.Marks)
		delete(st.Assignments, dep)
		markRemoved(st, dep, r.GetRevision(), now)
		if app != "" {
			queueRemoval(st, app)
		}
		markCommitted(st, entryID)
	}); err != nil {
		return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "save", err
	}
	p.stopPull(dep)
	if app != "" {
		if err := p.removeReleases(ctx, app); err != nil {
			return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "remove", err
		}
	}
	return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED, "", nil
}

// applyDetach ends a deployment on this device without removing its app
// (DetachDeviceRequest, service-protos #193). A pull of the deployment stops
// first; a start it had begun runs to its end (started, or the last applied
// app put back), so the device holds an app it applied, never neither. Then,
// as one state change on disk: the maximum revision becomes the deployment's
// removal mark, held or not, exactly as a terminal removal, so whichever of
// the two applies first wins and the later one deletes nothing; the held
// statement goes with its device wrap of the deployment key, which drops the
// key material and the pull right; and the app is recorded as detached,
// with the statement it runs stripped of every key (detachedStatement).
// The container, its image, configuration, run state and restart
// behaviour (its own restart policy and stop label) are left as they are.
// Only release images no container was created from are then pruned.
// From then on the app is a direct run: not held, so a new deployment of
// the app replaces it (applyAssignment).
func (p *Postbox) applyDetach(ctx context.Context, entryID int64, r *cloudpbv2.DetachDeviceRequest) (string, cloudpbv2.ApplyOutcome, string, error) {
	dep := r.GetDeploymentId()
	held := p.heldStatement(dep)
	if held != nil && held.AppID != r.GetAppId() {
		return refused(held.AppID, notForThisDevice("app", "detach names app %q; deployment %q here is app %q", r.GetAppId(), dep, held.AppID))
	}
	app := ""
	var kept []byte
	if held != nil {
		app = held.AppID
		p.stopPull(dep)
		held = p.heldStatement(dep) // as the pull left it
		a := &cloudpbv2.DeviceAssignment{}
		runs := proto.Unmarshal(held.Assignment, a) == nil && p.runsStatement(p.control.AppState(ctx, app), a)
		kept = detachedStatement(held, runs)
		if !held.Pulled && !runs {
			// The wire carries one outcome per app, and the detach's is the
			// newer: the interrupted Assign's end is recorded here.
			p.logger.Info("registry pull superseded by a detach", zap.String("audit", "postbox"),
				zap.Int64("entry_id", held.EntryID), zap.String("correlation_id", held.CorrelationID),
				zap.String("deployment_id", dep), zap.String("app_id", app),
				zap.String("outcome", cloudpbv2.ApplyOutcome_APPLY_OUTCOME_SUPERSEDED.String()))
		}
	}
	now := p.trustedNow()
	if err := p.commit(func(st *postboxState) {
		st.Assignments = maps.Clone(st.Assignments)
		st.Marks = maps.Clone(st.Marks)
		delete(st.Assignments, dep)
		markRemoved(st, dep, maxRevision, now)
		if app != "" {
			// An applied ABSENT revision is what it last applied: its removal
			// goes on, nothing is kept.
			if held.DesiredState != cloudpbv2.Running_ABSENT.String() {
				st.Detached = maps.Clone(st.Detached)
				if st.Detached == nil {
					st.Detached = map[string][]byte{}
				}
				st.Detached[app] = kept
			}
			queueRemoval(st, app) // the prune below, retried after a crash
		}
		markCommitted(st, entryID)
	}); err != nil {
		return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "save", err
	}
	if app == "" {
		return "", cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED, "", nil
	}
	if err := p.removeReleases(ctx, app); err != nil {
		return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "remove", err
	}
	return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED, "", nil
}

func (p *Postbox) heldStatement(dep string) *postboxAssignment {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state.Assignments[dep]
}

// detachedStatement is what a detached app runs: the held statement when it
// runs (or was applied), else the last applied one before it; nil for none.
// Only what a put-back needs is kept (app, release, signed digest,
// configuration, desired state), never the key id or the device wrap.
func detachedStatement(held *postboxAssignment, runs bool) []byte {
	raw := held.Previous
	if runs || held.Applied {
		raw = held.Assignment
	}
	a := &cloudpbv2.DeviceAssignment{}
	if len(raw) == 0 || proto.Unmarshal(raw, a) != nil {
		return nil
	}
	out, err := proto.Marshal(&cloudpbv2.DeviceAssignment{
		DeploymentId: a.GetDeploymentId(), AppId: a.GetAppId(), AppReleaseId: a.GetAppReleaseId(),
		ManifestDigest: a.GetManifestDigest(), ConfigJson: a.GetConfigJson(), DesiredState: a.GetDesiredState(),
	})
	if err != nil {
		return nil
	}
	return out
}
