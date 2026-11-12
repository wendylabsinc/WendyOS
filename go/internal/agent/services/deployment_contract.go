package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"strings"
	"time"

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
// and nothing held changes.
type contractRefusal struct {
	outcome cloudpbv2.ApplyOutcome
	reason  string
	err     error
}

func (r *contractRefusal) Error() string { return r.err.Error() }

func stale(reason, format string, args ...any) error {
	return &contractRefusal{cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE, reason, fmt.Errorf(format, args...)}
}

func notForThisDevice(reason, format string, args ...any) error {
	return &contractRefusal{cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_TARGET, reason, fmt.Errorf(format, args...)}
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
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return errors.New("the configuration is not a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("the configuration is more than one JSON object")
	}
	return nil
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
			return false, stale("revision", "deployment was removed at revision %d; assignment revision %d", m.Revision, rev)
		case rev < m.Revision:
			return false, stale("revision", "assignment revision %d is below the accepted revision %d", rev, m.Revision)
		case rev == m.Revision:
			held := st.Assignments[dep]
			prev := &cloudpbv2.DeviceAssignment{}
			if held == nil || proto.Unmarshal(held.Assignment, prev) != nil || !sameContent(prev, a) {
				return false, stale("revision_conflict", "assignment revision %d differs from the one already accepted", rev)
			}
			return true, nil
		}
	}
	if err := checkConfig(a.GetConfigJson()); err != nil {
		return false, &contractRefusal{cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "config", err}
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

// markRemoved records a removal mark and drops removal marks past
// removalMarkKeep.
func markRemoved(st *postboxState, dep string, rev int32, now time.Time) {
	for d, m := range st.Marks {
		if m.Removed && time.Unix(m.RemovedAt, 0).Add(removalMarkKeep).Before(now) {
			delete(st.Marks, d)
		}
	}
	st.Marks[dep] = &postboxMark{Revision: rev, Removed: true, RemovedAt: now.Unix()}
}

// refused turns a contract refusal into apply's result.
func refused(app string, err error) (string, cloudpbv2.ApplyOutcome, string, error) {
	var r *contractRefusal
	if errors.As(err, &r) {
		return app, r.outcome, r.reason, r.err
	}
	return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "assignment", err
}

// applyAssignment holds a verified assignment the contract accepts, as one
// state change: the mark moves, the statement is held, and a move's old
// deployment leaves with its removal mark. Its pull then runs beside the
// queue. A refused assignment changes nothing held.
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
	// A newer entry for the deployment replaces its pull, and a move stops
	// the old deployment's.
	p.stopPull(dep)
	from := r.GetReplacesDeploymentId()
	if from != "" && !repeat {
		p.stopPull(from)
	}
	now := p.trustedNow()
	absent := r.GetDesiredState() == cloudpbv2.Running_ABSENT
	p.saveMu.Lock() // a pull's commit must not swap this away; decide saves it
	p.mu.Lock()
	st := p.state
	held := &postboxAssignment{
		AppID: app, AppReleaseID: r.GetAppReleaseId(), ManifestDigest: r.GetManifestDigest(),
		DeploymentKeyID: r.GetDeploymentKeyId(), DesiredState: r.GetDesiredState().String(),
		Assignment: raw, EntryID: entryID, CorrelationID: correlation, Pulled: absent,
	}
	outcome := outcomePulling
	if repeat {
		// Key rotation: the new statement (wrap, key id, expiry) is held and
		// nothing is applied again. A pull that ended keeps its outcome.
		prev := st.Assignments[dep]
		held.Pulled = prev.Pulled
		if o := st.Outcomes[app]; prev.Pulled && o != nil && o.EntryID == prev.EntryID {
			outcome = o.Outcome
		} else if prev.Pulled {
			outcome = cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED
		}
	}
	st.Assignments = maps.Clone(st.Assignments) // copy on write: a pull's commit reads it
	st.Marks = maps.Clone(st.Marks)
	st.Assignments[dep] = held
	st.Marks[dep] = &postboxMark{Revision: r.GetRevision()}
	if from != "" && !repeat {
		delete(st.Assignments, from)
		markRemoved(st, from, r.GetReplacesRevision(), now)
	}
	p.mu.Unlock()
	p.saveMu.Unlock()
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

// applyUnassign removes a deployment the contract lets go: the mark becomes
// a removal mark, its pull stops before anything is removed (so nothing it
// imports outlives the deployment) and the app's releases go. A duplicate
// removal is consumed without effect.
func (p *Postbox) applyUnassign(ctx context.Context, r *cloudpbv2.UnassignDeviceRequest) (string, cloudpbv2.ApplyOutcome, string, error) {
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
	p.stopPull(dep)
	now := p.trustedNow()
	p.saveMu.Lock()
	p.mu.Lock()
	app := ""
	if a := p.state.Assignments[dep]; a != nil {
		app = a.AppID
	}
	p.state.Assignments = maps.Clone(p.state.Assignments)
	p.state.Marks = maps.Clone(p.state.Marks)
	delete(p.state.Assignments, dep)
	markRemoved(p.state, dep, r.GetRevision(), now)
	p.mu.Unlock()
	p.saveMu.Unlock()
	if app != "" {
		if err := p.removeReleases(ctx, app); err != nil {
			return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "remove", err
		}
	}
	return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED, "", nil
}
