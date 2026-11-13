package services

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// statement signs an assignment (a non-nil assign) or a removal for dep
// under method and queues it.
func (f *pbFixture) statement(method, dep string, assign *cloudpbv2.DeviceAssignment, removalRev int32) {
	f.t.Helper()
	resource := "org/" + f.tenant + "/deployment/" + dep + "/device/" + f.uuid
	var jws, payload []byte
	var typ string
	if assign != nil {
		jws, payload, typ = f.sign(method, assign, resource, []*x509.Certificate{f.opLeaf, f.opCA}, f.opKey, nil)
	} else {
		req := &cloudpbv2.UnassignDeviceRequest{OrganizationId: f.tenant, DeploymentId: dep, Device: f.uri,
			ValidUntil: timestamppb.New(f.now.Add(time.Hour)), Revision: removalRev}
		jws, payload, typ = f.sign(method, req, resource, []*x509.Certificate{f.opLeaf, f.opCA}, f.opKey, nil)
	}
	f.cloud.add(jws, payload, typ)
}

// assignment is a valid assignment of app at rev; the pull it starts is
// refused (no registry in the fixture), which these tests do not look at.
func (f *pbFixture) assignment(dep, app string, rev int32) *cloudpbv2.DeviceAssignment {
	return &cloudpbv2.DeviceAssignment{
		OrganizationId: f.tenant, DeploymentId: dep, Device: f.uri, AppId: app, AppReleaseId: uuid.NewString(),
		ManifestDigest: "sha256:ab", DeploymentKeyId: "k1", DesiredState: cloudpbv2.Running_STOPPED,
		ExpiresAt: timestamppb.New(f.now.Add(time.Hour)), ConfigJson: `{"env":{"A":"1"}}`, Revision: rev,
	}
}

var (
	assignNew   = cloudpbv2.DeploymentService_AssignDevice_FullMethodName
	assignOld   = cloudpbv2.ArtifactRegistryService_AssignDevice_FullMethodName
	unassignNew = cloudpbv2.DeploymentService_UnassignDevice_FullMethodName
	unassignOld = cloudpbv2.ArtifactRegistryService_UnassignDevice_FullMethodName
)

func (f *pbFixture) mark(dep string) *postboxMark { return f.p.snapshot().Marks[dep] }

func (f *pbFixture) held(dep string) *postboxAssignment { return f.p.snapshot().Assignments[dep] }

// The allow-list is exactly the four relayed methods plus the two moved
// names, so the deployment-level changes can never be applied on a device.
func TestPostboxOperationsAllowList(t *testing.T) {
	var got []string
	for op := range postboxOperations {
		got = append(got, op)
	}
	slices.Sort(got)
	want := []string{
		"wendycloud.v2.ArtifactRegistryService/AssignDevice",
		"wendycloud.v2.ArtifactRegistryService/UnassignDevice",
		"wendycloud.v2.DeploymentService/AssignDevice",
		"wendycloud.v2.DeploymentService/ControlContainer",
		"wendycloud.v2.DeploymentService/CreateDeployment",
		"wendycloud.v2.DeploymentService/UnassignDevice",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("allow-list %v, want %v", got, want)
	}
}

// Both names apply, each only with its own payload type; a deployment-level
// method is refused as not a device action.
func TestPostboxAcceptsBothOperationNames(t *testing.T) {
	f := newPBFixture(t)
	a, b := uuid.NewString(), uuid.NewString()
	f.statement(assignNew, a, f.assignment(a, "web", 1), 0)
	f.statement(assignOld, b, f.assignment(b, "db", 1), 0)
	f.statement(unassignNew, a, nil, maxRevision)
	f.statement(unassignOld, b, nil, maxRevision)
	// The new name with the other statement's payload type.
	c := uuid.NewString()
	f.statement(unassignNew, c, f.assignment(c, "api", 1), 0)
	f.statement(cloudpbv2.DeploymentService_RemoveDeployment_FullMethodName, c, nil, maxRevision)
	f.cycle()
	if f.mark(a) == nil || !f.mark(a).Removed || f.mark(b) == nil || !f.mark(b).Removed {
		t.Fatalf("marks %v: want both names applied", f.p.snapshot().Marks)
	}
	if f.held(c) != nil || f.mark(c) != nil || f.p.refusals["operation"] != 2 {
		t.Fatalf("held %v, mark %v, refusals %v: want the mismatched type and RemoveDeployment refused", f.held(c), f.mark(c), f.p.refusals)
	}
}

func TestPostboxRefusesRevisionZeroAndTheMaximum(t *testing.T) {
	f := newPBFixture(t)
	a, b := uuid.NewString(), uuid.NewString()
	f.statement(assignNew, a, f.assignment(a, "web", 0), 0)
	f.statement(assignNew, b, f.assignment(b, "db", maxRevision), 0)
	f.statement(unassignNew, a, nil, 0)
	f.cycle()
	if len(f.p.snapshot().Assignments) != 0 || len(f.p.snapshot().Marks) != 0 || f.p.refusals["revision"] != 3 {
		t.Fatalf("held %v, marks %v, refusals %v", f.p.snapshot().Assignments, f.p.snapshot().Marks, f.p.refusals)
	}
	if o := f.outcome("web").Outcome; o != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE {
		t.Fatalf("outcome %v, want REJECTED_STALE", o)
	}
}

// The mark survives a removal and a restart; a terminal removal is
// permanent; an older or equal statement never comes back.
func TestPostboxRevisionMarkKeptAcrossRemovalAndRestart(t *testing.T) {
	f := newPBFixture(t)
	dep := uuid.NewString()
	f.statement(assignNew, dep, f.assignment(dep, "web", 3), 0)
	f.statement(unassignNew, dep, nil, 4) // non-terminal
	f.cycle()
	f.p.mu.Lock()
	f.p.state = nil // restart: state comes back from disk
	f.p.mu.Unlock()
	f.statement(assignNew, dep, f.assignment(dep, "web", 4), 0) // equal to the removal mark
	f.statement(assignNew, dep, f.assignment(dep, "web", 2), 0) // below
	f.cycle()
	if m := f.mark(dep); m == nil || m.Revision != 4 || !m.Removed || f.held(dep) != nil {
		t.Fatalf("mark %+v, held %+v: want the removal mark kept and both refused", m, f.held(dep))
	}
	// Taken back at a later revision, then removed for good.
	f.statement(assignNew, dep, f.assignment(dep, "web", 5), 0)
	f.cycle()
	if f.held(dep) == nil {
		t.Fatal("a later revision did not take the device back")
	}
	f.statement(unassignNew, dep, nil, maxRevision)
	f.statement(assignNew, dep, f.assignment(dep, "web", 6), 0)
	f.cycle()
	if m := f.mark(dep); m.Revision != maxRevision || !m.Removed || f.held(dep) != nil {
		t.Fatalf("mark %+v, held %+v: want a terminal removal to be permanent", m, f.held(dep))
	}
}

// Removal marks are kept for at least a year and then dropped, measured
// against the persisted time floor: a clock run forward drops nothing.
func TestRemovalMarksArePrunedAfterTheyCanMatter(t *testing.T) {
	floor := time.Now()
	st := &postboxState{TimeFloor: floor.Unix(), Marks: map[string]*postboxMark{
		"recent": {Revision: 2, Removed: true, RemovedAt: floor.Add(-366 * 24 * time.Hour).Unix()},
		"old":    {Revision: 2, Removed: true, RemovedAt: floor.Add(-removalMarkKeep - time.Hour).Unix()},
		"live":   {Revision: 1},
	}}
	markRemoved(st, "new", 3, floor.Add(10*removalMarkKeep)) // the clock is far ahead
	if st.Marks["recent"] == nil || st.Marks["old"] != nil || st.Marks["live"] == nil || st.Marks["new"] == nil {
		t.Fatalf("marks %v", st.Marks)
	}
}

// At an equal revision only identical content is accepted (a key
// rotation): the new statement is held and the move is not applied again.
func TestPostboxEqualRevisionAcceptsOnlyIdenticalContent(t *testing.T) {
	f := newPBFixture(t)
	old, dep := uuid.NewString(), uuid.NewString()
	f.statement(assignNew, old, f.assignment(old, "web", 1), 0)
	f.cycle()
	move := f.assignment(dep, "web", 1)
	move.ReplacesDeploymentId, move.ReplacesRevision = old, 2
	f.statement(assignNew, dep, move, 0)
	f.cycle()
	removedAt := f.mark(old).RemovedAt
	// A rotated copy of the move, later: held, the move not applied
	// again (the old deployment's removal mark keeps its time).
	f.p.now = func() time.Time { return f.now.Add(30 * time.Minute) }
	rotated := proto.Clone(move).(*cloudpbv2.DeviceAssignment)
	rotated.DeploymentKeyId = "k2"
	f.statement(assignNew, dep, rotated, 0)
	f.cycle()
	if h := f.held(dep); h == nil || h.DeploymentKeyID != "k2" || f.mark(old).RemovedAt != removedAt {
		t.Fatalf("held %+v, old mark %+v: want the rotation held and the move not applied again", h, f.mark(old))
	}

	g := newPBFixture(t)
	d := uuid.NewString()
	first := g.assignment(d, "web", 1)
	g.statement(assignNew, d, first, 0)
	g.cycle()
	rotated = g.assignment(d, "web", 1)
	rotated.AppReleaseId, rotated.DeploymentKeyId = first.AppReleaseId, "k2"
	changed := g.assignment(d, "web", 1)
	changed.AppReleaseId = first.AppReleaseId
	changed.ConfigJson = `{"env":{"A":"2"}}`
	g.statement(assignNew, d, rotated, 0)
	g.statement(assignNew, d, changed, 0)
	g.cycle()
	if h := g.held(d); h == nil || h.DeploymentKeyID != "k2" || g.p.refusals["revision_conflict"] != 1 {
		t.Fatalf("held %+v, refusals %v: want the rotation held and the changed copy refused", h, g.p.refusals)
	}
}

// A move removes the replaced deployment and installs the new one in one
// state change; its checks hold; a removal that arrived first counts as
// the old side done.
func TestPostboxMove(t *testing.T) {
	f := newPBFixture(t)
	old, dep := uuid.NewString(), uuid.NewString()
	f.statement(assignNew, old, f.assignment(old, "web", 1), 0)
	f.cycle()
	move := f.assignment(dep, "web", 1)
	move.ReplacesDeploymentId, move.ReplacesRevision = old, 2
	f.statement(assignNew, dep, move, 0)
	f.cycle()
	if f.held(old) != nil || f.held(dep) == nil || !f.mark(old).Removed || f.mark(old).Revision != 2 || f.mark(dep).Revision != 1 {
		t.Fatalf("held %v, marks %v: want the move applied", f.p.snapshot().Assignments, f.p.snapshot().Marks)
	}

	g := newPBFixture(t)
	gold, gdep, other := uuid.NewString(), uuid.NewString(), uuid.NewString()
	g.statement(assignNew, gold, g.assignment(gold, "web", 1), 0)
	g.statement(assignNew, other, g.assignment(other, "db", 1), 0)
	g.statement(unassignNew, gold, nil, 2) // the old side arrives first
	g.cycle()
	bad := []*cloudpbv2.DeviceAssignment{g.assignment(gdep, "web", 1), g.assignment(gdep, "web", 1), g.assignment(gdep, "web", 1)}
	bad[0].ReplacesDeploymentId, bad[0].ReplacesRevision = other, 2 // other is app db
	bad[1].ReplacesDeploymentId, bad[1].ReplacesRevision = gold, 1  // below the removal mark
	bad[2].ReplacesDeploymentId, bad[2].ReplacesRevision = gdep, 2  // itself
	for _, a := range bad {
		g.statement(assignNew, gdep, a, 0)
	}
	g.cycle()
	if g.held(gdep) != nil || g.held(other) == nil || g.p.refusals["move"] != 3 {
		t.Fatalf("held %v, refusals %v: want the three bad moves refused", g.p.snapshot().Assignments, g.p.refusals)
	}
	done := g.assignment(gdep, "web", 1)
	done.ReplacesDeploymentId, done.ReplacesRevision = gold, 2 // equal to the removal mark
	g.statement(assignNew, gdep, done, 0)
	g.cycle()
	if g.held(gdep) == nil {
		t.Fatal("old side already done: want the new deployment installed")
	}
}

// One instance of an app per device: a second deployment of the app is
// refused unless it is a move from the first.
func TestPostboxOneInstancePerApp(t *testing.T) {
	f := newPBFixture(t)
	a, b := uuid.NewString(), uuid.NewString()
	f.statement(assignNew, a, f.assignment(a, "web", 1), 0)
	f.statement(assignNew, b, f.assignment(b, "web", 1), 0)
	f.cycle()
	if f.held(b) != nil || f.mark(b) != nil || f.p.refusals["one_instance"] != 1 {
		t.Fatalf("held %v, refusals %v", f.p.snapshot().Assignments, f.p.refusals)
	}
	if o := f.outcome("web").Outcome; o != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_TARGET {
		t.Fatalf("outcome %v, want REJECTED_TARGET", o)
	}
}

// A removal equal to the removal mark is consumed without effect; one at
// or below an assignment's mark is refused.
func TestPostboxDuplicateRemovalIsANoOp(t *testing.T) {
	f := newPBFixture(t)
	dep := uuid.NewString()
	f.statement(assignNew, dep, f.assignment(dep, "web", 2), 0)
	f.statement(unassignNew, dep, nil, 2) // not above the assignment
	f.cycle()
	if f.held(dep) == nil || f.p.refusals["revision"] != 1 {
		t.Fatalf("held %v, refusals %v: want the removal at the assignment's revision refused", f.held(dep), f.p.refusals)
	}
	f.statement(unassignNew, dep, nil, 3)
	f.statement(unassignNew, dep, nil, 3)
	f.cycle()
	if m := f.mark(dep); m == nil || !m.Removed || m.Revision != 3 || f.p.refusals["revision"] != 1 {
		t.Fatalf("mark %+v, refusals %v: want the duplicate consumed without a refusal", m, f.p.refusals)
	}
}

func TestCheckConfig(t *testing.T) {
	for config, ok := range map[string]bool{
		`{}`:                  true,
		` {"env":{"A":"1"}} `: true,
		``:                    false,
		`null`:                false,
		`[]`:                  false,
		`"x"`:                 false,
		`{} {}`:               false,
		`{"a":1`:              false,
		`{"a":"` + strings.Repeat("x", maxConfigSize) + `"}`: false,
		`{"a":{"a":1},"b":[{"a":1},{"a":2}]}`:                true,
		`{"a":1,"a":2}`:                                      false,
		`{"b":{"a":1,"a":2}}`:                                false,
		`{"c":[{"a":1,"a":2}]}`:                              false,
		`{"a":}`:                                             false,
		`{"env":1,"ENV":2}`:                                  false,
		`{"k":1,"\u212a":2}`:                                 false, // Kelvin sign folds to k
		`{"a":{"env":1},"b":{"ENV":1}}`:                      true,
		`{"a":1e400}`:                                        true,
		`{"a":123456789012345678901234567890}`:               true,
	} {
		if err := checkConfig(config); (err == nil) != ok {
			t.Errorf("checkConfig(%.20q) = %v, want ok %v", config, err, ok)
		}
	}
}

func TestPostboxRefusesAnInvalidConfiguration(t *testing.T) {
	f := newPBFixture(t)
	dep := uuid.NewString()
	a := f.assignment(dep, "web", 1)
	a.ConfigJson = `[1]`
	f.statement(assignNew, dep, a, 0)
	f.cycle()
	if f.held(dep) != nil || f.mark(dep) != nil || f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED || f.p.refusals["config"] != 1 {
		t.Fatalf("held %v, outcome %v, refusals %v", f.held(dep), f.outcome("web"), f.p.refusals)
	}
}

// Cloud caps a page at 2 MiB: fewer entries with has_more. The cycle keeps
// fetching until it has every entry.
func TestPostboxFollowsCappedPages(t *testing.T) {
	f := newPBFixture(t)
	f.cloud.pageSize = 2
	for _, app := range []string{"a1", "a2", "a3", "a4", "a5"} {
		f.control(app, cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
	}
	f.cycle()
	if hw := f.p.snapshot().HighWater; hw != 5 {
		t.Fatalf("high-water %d, want all 5 entries in one cycle", hw)
	}
}

// The old relayed CreateDeployment is never applied.
func TestPostboxNeverAppliesCreateDeployment(t *testing.T) {
	f := newPBFixture(t)
	req := &cloudpbv2.CreateDeploymentRequest{OrganizationId: f.tenant, AppReleaseId: "rel", TargetDeviceIds: []string{f.uuid}, ValidUntil: timestamppb.New(f.now.Add(time.Hour))}
	jws, payload, typ := f.sign(cloudpbv2.DeploymentService_CreateDeployment_FullMethodName, req, "org/"+f.tenant+"/release/rel", []*x509.Certificate{f.opLeaf, f.opCA}, f.opKey, nil)
	f.cloud.add(jws, payload, typ)
	f.cycle()
	if f.p.refusals["material"] != 1 || len(f.ctr.startedNames) != 0 || len(f.p.snapshot().Assignments) != 0 {
		t.Fatalf("refusals %v, starts %v: want CreateDeployment refused and nothing applied", f.p.refusals, f.ctr.startedNames)
	}
}

// The transient outcomes are not sent until the switch is turned on.
func TestTransientOutcomesAreOffByDefault(t *testing.T) {
	if sendTransientOutcomes {
		t.Fatal("sendTransientOutcomes must stay off until cloud on prod accepts the values")
	}
	for _, o := range []cloudpbv2.ApplyOutcome{outcomePulling, outcomeNotStarted, outcomePullRetrying} {
		if o != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_UNSPECIFIED {
			t.Fatalf("outcome %v sent while the switch is off", o)
		}
	}
	if transientOutcome(cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULLING) != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_UNSPECIFIED {
		t.Fatal("PULLING mapped while off")
	}
}

// Each content field and move rule, alone: every case fails without its
// rule. The held statement is deployment "d" at revision 2, a move from
// "x" at 1; "old" is another deployment of the app held at revision 3.
func TestAssignOrderRules(t *testing.T) {
	base := func() *cloudpbv2.DeviceAssignment {
		return &cloudpbv2.DeviceAssignment{
			DeploymentId: "d", AppId: "web", AppReleaseId: "r1", ManifestDigest: "sha256:aa", ConfigJson: "{}",
			DesiredState: cloudpbv2.Running_RUNNING, Revision: 2, ReplacesDeploymentId: "x", ReplacesRevision: 1,
		}
	}
	raw, err := proto.Marshal(base())
	if err != nil {
		t.Fatal(err)
	}
	state := func() *postboxState {
		return &postboxState{
			Assignments: map[string]*postboxAssignment{"d": {AppID: "web", Assignment: raw}},
			Marks:       map[string]*postboxMark{"d": {Revision: 2}, "x": {Revision: 1, Removed: true}, "old": {Revision: 3}},
		}
	}
	for name, tc := range map[string]struct {
		change func(*cloudpbv2.DeviceAssignment)
		reason string // "" = accepted
	}{
		"rotation":            {func(a *cloudpbv2.DeviceAssignment) { a.DeploymentKeyId = "k2" }, ""},
		"desired state":       {func(a *cloudpbv2.DeviceAssignment) { a.DesiredState = cloudpbv2.Running_STOPPED }, "revision_conflict"},
		"release":             {func(a *cloudpbv2.DeviceAssignment) { a.AppReleaseId = "r2" }, "revision_conflict"},
		"digest":              {func(a *cloudpbv2.DeviceAssignment) { a.ManifestDigest = "sha256:bb" }, "revision_conflict"},
		"config":              {func(a *cloudpbv2.DeviceAssignment) { a.ConfigJson = `{"a":1}` }, "revision_conflict"},
		"replaces deployment": {func(a *cloudpbv2.DeviceAssignment) { a.ReplacesDeploymentId = "y" }, "revision_conflict"},
		"replaces revision":   {func(a *cloudpbv2.DeviceAssignment) { a.ReplacesRevision = 3 }, "revision_conflict"},
		"move above the mark": {func(a *cloudpbv2.DeviceAssignment) {
			a.Revision, a.ReplacesDeploymentId, a.ReplacesRevision = 3, "old", 4
		}, "one_instance"},
		"move at its mark": {func(a *cloudpbv2.DeviceAssignment) {
			a.Revision, a.ReplacesDeploymentId, a.ReplacesRevision = 3, "old", 3
		}, "move"},
		"move at the maximum": {func(a *cloudpbv2.DeviceAssignment) {
			a.Revision, a.ReplacesDeploymentId, a.ReplacesRevision = 3, "old", maxRevision
		}, "move"},
		"move done already": {func(a *cloudpbv2.DeviceAssignment) { a.Revision, a.ReplacesRevision = 3, 1 }, ""},
	} {
		a := base()
		tc.change(a)
		st := state()
		if name == "move above the mark" {
			// Accepted as a move once "d" is not also held for the app.
			delete(st.Assignments, "d")
			st.Marks["d"] = &postboxMark{Revision: 2}
			tc.reason = ""
		}
		_, err := assignOrder(st, a)
		var r *contractRefusal
		switch {
		case tc.reason == "" && err != nil:
			t.Errorf("%s: refused: %v", name, err)
		case tc.reason != "" && (!errors.As(err, &r) || r.reason != tc.reason):
			t.Errorf("%s: got %v, want refusal %q", name, err, tc.reason)
		}
	}
}

// diskPeek snapshots postbox.json while the irreversible removal runs: a
// crash at that instant leaves exactly this file (the verifier's probe).
type diskPeek struct {
	postboxControl
	dir  string
	snap []byte
}

func (d *diskPeek) RemoveReleases(ctx context.Context, app string, keep []string) error {
	d.snap, _ = os.ReadFile(filepath.Join(d.dir, postboxStateFile))
	return d.postboxControl.RemoveReleases(ctx, app, keep)
}

// A removal is on disk before anything is deleted: a crash during the
// delete keeps the removal (held statement gone, removal mark, app queued)
// and the restart finishes it. The same for an ABSENT revision.
func TestPostboxRemovalIsDurableBeforeItActs(t *testing.T) {
	for _, absent := range []bool{false, true} {
		f := newPBFixture(t)
		peek := &diskPeek{postboxControl: f.p.control, dir: f.dir}
		f.p.control = peek
		dep := uuid.NewString()
		f.statement(assignNew, dep, f.assignment(dep, "web", 1), 0)
		f.cycle()
		f.p.waitPulls()
		peek.snap = nil
		if absent {
			a := f.assignment(dep, "web", 2)
			a.DesiredState = cloudpbv2.Running_ABSENT
			f.statement(assignNew, dep, a, 0)
		} else {
			f.statement(unassignNew, dep, nil, 2)
		}
		f.cycle()
		var st postboxState
		if err := json.Unmarshal(peek.snap, &st); err != nil {
			t.Fatalf("absent %v: no state on disk during the removal: %v", absent, err)
		}
		held := st.Assignments[dep]
		removedOnDisk := !absent && held == nil && st.Marks[dep] != nil && st.Marks[dep].Removed && st.Marks[dep].Revision == 2 ||
			absent && held != nil && held.DesiredState == cloudpbv2.Running_ABSENT.String() && st.Marks[dep].Revision == 2
		if !removedOnDisk || !slices.Contains(st.Removals, "web") {
			t.Fatalf("absent %v: on disk during the delete: held %+v, mark %+v, removals %v", absent, held, st.Marks[dep], st.Removals)
		}
		// Crash there and restart: the queued removal runs again.
		if err := os.WriteFile(filepath.Join(f.dir, postboxStateFile), peek.snap, 0o600); err != nil {
			t.Fatal(err)
		}
		f.p.mu.Lock()
		f.p.state = nil
		f.p.mu.Unlock()
		peek.snap = nil
		f.cycle()
		f.p.waitPulls()
		if peek.snap == nil || slices.Contains(f.p.snapshot().Removals, "web") {
			t.Fatalf("absent %v: removal not finished after the restart (removals %v)", absent, f.p.snapshot().Removals)
		}
	}
}

// A same-revision copy after a refused pull is pulled again; after a
// successful one it is not.
func TestPostboxRotationAfterARefusedPullPullsAgain(t *testing.T) {
	f := newPBFixture(t)
	d := uuid.NewString()
	first := f.assignment(d, "web", 1)
	f.statement(assignNew, d, first, 0)
	f.cycle()
	f.p.waitPulls()
	refused := f.p.refusals["pull"]
	if f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULL_REFUSED || refused == 0 {
		t.Fatalf("first pull: outcome %v, refusals %v", f.outcome("web"), f.p.refusals)
	}
	rot := proto.Clone(first).(*cloudpbv2.DeviceAssignment)
	rot.DeploymentKeyId = "k2"
	f.statement(assignNew, d, rot, 0)
	f.cycle()
	f.p.waitPulls()
	if f.p.refusals["pull"] != refused+1 {
		t.Fatalf("refusals %v: want the copy pulled again", f.p.refusals)
	}

	g := newPBFixture(t)
	r := g.release(t)
	r.assign.Revision, r.assign.ConfigJson = 1, "{}"
	rec := g.withArtifacts(&fakeArtifacts{r: r})
	g.assignEntry(r.assign)
	g.cycle()
	g.p.waitPulls()
	again := proto.Clone(r.assign).(*cloudpbv2.DeviceAssignment)
	again.ExpiresAt = timestamppb.New(g.now.Add(2 * time.Hour))
	g.assignEntry(again)
	g.cycle()
	g.p.waitPulls()
	if len(rec.imported) != 1 || g.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
		t.Fatalf("imports %d, outcome %v: want an applied pull kept, not repeated", len(rec.imported), g.outcome("web"))
	}
}

// A null entry in the saved state holds the postbox: nothing is applied.
func TestPostboxNullStateEntryFailsSafe(t *testing.T) {
	for _, field := range []string{"marks", "assignments", "outcomes"} {
		f := newPBFixture(t)
		data := `{"principal":"` + f.uri + `","` + field + `":{"x":null}}`
		if err := os.WriteFile(filepath.Join(f.dir, postboxStateFile), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
		if err := f.p.cycle(context.Background()); err == nil || len(f.ctr.stopCalls) != 0 {
			t.Fatalf("%s: err %v, stops %v: want the postbox held", field, err, f.ctr.stopCalls)
		}
	}
}

// Every crash point of a removal: restarting from each state write either
// finishes the removal and reports it applied, or (crash after the claim,
// before the commit) reports the entry failed against the app. The old
// deployment never comes back.
func TestPostboxRemovalCrashPoints(t *testing.T) {
	for _, kind := range []string{"unassign", "terminal", "absent"} {
		for k := 0; ; k++ {
			f := newPBFixture(t)
			dep := uuid.NewString()
			first := f.assignment(dep, "web", 1)
			f.statement(assignNew, dep, first, 0)
			f.cycle()
			f.p.waitPulls()
			var writes [][]byte
			write := f.p.write
			f.p.write = func(dir, name string, data []byte) error {
				writes = append(writes, slices.Clone(data))
				return write(dir, name, data)
			}
			switch kind {
			case "unassign":
				f.statement(unassignNew, dep, nil, 2)
			case "terminal":
				f.statement(unassignNew, dep, nil, maxRevision)
			case "absent":
				a := f.assignment(dep, "web", 2)
				a.DesiredState = cloudpbv2.Running_ABSENT
				f.statement(assignNew, dep, a, 0)
			}
			f.cycle()
			f.p.waitPulls()
			if k >= len(writes) {
				break
			}
			f.p.write = write
			var snap postboxState
			if err := json.Unmarshal(writes[k], &snap); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.dir, postboxStateFile), writes[k], 0o600); err != nil {
				t.Fatal(err)
			}
			f.p.mu.Lock()
			f.p.state = nil
			f.p.mu.Unlock()
			f.cycle()
			f.p.waitPulls()
			o := f.outcome("web")
			removed := len(f.p.snapshot().Removals) == 0 && o.EntryID == 2 && o.Outcome == cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED &&
				(kind == "absent" || f.held(dep) == nil && f.mark(dep).Removed)
			// Only a crash before the removal reached the disk may lose it.
			onDisk := snap.Marks[dep] != nil && snap.Marks[dep].Revision != 1
			interrupted := !onDisk && snap.Pending != nil &&
				o.EntryID == 2 && o.Outcome == cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED
			if !removed && !interrupted {
				t.Fatalf("%s, crash after write %d (pending %+v): outcome %+v, held %+v, mark %+v, removals %v",
					kind, k, snap.Pending, o, f.held(dep), f.mark(dep), f.p.snapshot().Removals)
			}
			// The old statement never comes back.
			again := proto.Clone(first).(*cloudpbv2.DeviceAssignment)
			again.ExpiresAt = timestamppb.New(f.now.Add(2 * time.Hour))
			f.statement(assignNew, dep, again, 0)
			f.cycle()
			f.p.waitPulls()
			if h := f.held(dep); removed && h != nil && h.DesiredState != cloudpbv2.Running_ABSENT.String() {
				t.Fatalf("%s, crash after write %d: the removed deployment came back", kind, k)
			}
		}
	}
}

// committedWrite runs one cycle for the queued entry and returns the state
// write that carries its committed change (Pending.Committed).
func (f *pbFixture) committedWrite() []byte {
	f.t.Helper()
	var writes [][]byte
	write := f.p.write
	f.p.write = func(dir, name string, data []byte) error {
		writes = append(writes, slices.Clone(data))
		return write(dir, name, data)
	}
	f.cycle()
	f.p.waitPulls()
	f.p.write = write
	for _, w := range writes {
		var st postboxState
		if json.Unmarshal(w, &st) == nil && st.Pending != nil && st.Pending.Committed {
			return w
		}
	}
	f.t.Fatal("no committed write")
	return nil
}

// restartFrom puts snap on disk as the state a crash left and runs a cycle.
func (f *pbFixture) restartFrom(snap []byte) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, postboxStateFile), snap, 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.p.mu.Lock()
	f.p.state = nil
	f.p.mu.Unlock()
	f.cycle()
	f.p.waitPulls()
}

// A refused removal does not overwrite the outcome of the deployment the
// device holds; it is counted and audited only.
func TestPostboxRefusedRemovalKeepsTheHeldOutcome(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	r.assign.Revision, r.assign.ConfigJson = 1, "{}"
	f.withArtifacts(&fakeArtifacts{r: r})
	f.assignEntry(r.assign)
	f.cycle()
	f.p.waitPulls()
	before := f.outcome("web")
	dep := r.assign.GetDeploymentId()
	expired := &cloudpbv2.UnassignDeviceRequest{OrganizationId: f.tenant, DeploymentId: dep, Device: f.uri,
		ValidUntil: timestamppb.New(f.now.Add(-time.Second)), Revision: 2}
	jws, payload, typ := f.sign(unassignNew, expired, "org/"+f.tenant+"/deployment/"+dep+"/device/"+f.uuid, []*x509.Certificate{f.opLeaf, f.opCA}, f.opKey, nil)
	f.cloud.add(jws, payload, typ)
	f.cycle()
	if o := f.outcome("web"); o != before || before.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED || f.held(dep) == nil {
		t.Fatalf("outcome %+v (before %+v): want the held deployment's outcome kept", o, before)
	}
}

// A crash after a rotation copy's commit never reports "applied" for an
// app that was not started.
func TestPostboxRotationCrashKeepsTheTrueOutcome(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	r.assign.Revision, r.assign.ConfigJson, r.assign.DesiredState = 1, "{}", cloudpbv2.Running_RUNNING
	f.withArtifacts(&fakeArtifacts{r: r})
	f.assignEntry(r.assign)
	f.cycle()
	f.p.waitPulls()
	if f.outcome("web").Outcome == cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
		t.Fatal("setup: a running release reported applied")
	}
	rot := proto.Clone(r.assign).(*cloudpbv2.DeviceAssignment)
	rot.ExpiresAt = timestamppb.New(f.now.Add(2 * time.Hour))
	f.assignEntry(rot)
	f.restartFrom(f.committedWrite())
	if o := f.outcome("web"); o.EntryID != 2 || o.Outcome == cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
		t.Fatalf("outcome %+v: want entry 2 settled without claiming applied", o)
	}
}

// A committed removal whose delete fails at restart is reported failed and
// stays queued.
func TestPostboxCommittedRemovalFailingAtRestart(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	r.assign.Revision, r.assign.ConfigJson = 1, "{}"
	rec := f.withArtifacts(&fakeArtifacts{r: r})
	f.assignEntry(r.assign)
	f.cycle()
	f.p.waitPulls()
	f.statement(unassignNew, r.assign.GetDeploymentId(), nil, 2)
	snap := f.committedWrite()
	rec.removeErr = errors.New("containerd down")
	f.restartFrom(snap)
	if o := f.outcome("web"); o.EntryID != 2 || o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED || !slices.Contains(f.p.snapshot().Removals, "web") {
		t.Fatalf("outcome %+v, removals %v: want the failed removal reported and kept queued", o, f.p.snapshot().Removals)
	}
}

// Ruling B: a refused statement that is not news (refused in verification,
// behind the accepted revision, or a conflicting copy of it) is audit only
// and keeps the held deployment's outcome. A refused newer revision
// (configuration, one instance, move) is reported against the app under its
// own entry. Either way the held deployment stays untouched.
func TestPostboxRefusalReporting(t *testing.T) {
	for name, tc := range map[string]struct {
		statement func(f *pbFixture, dep string) (string, *cloudpbv2.DeviceAssignment)
		reported  cloudpbv2.ApplyOutcome // UNSPECIFIED = not reported
	}{
		"expired copy (verification)": {func(f *pbFixture, dep string) (string, *cloudpbv2.DeviceAssignment) {
			a := f.assignment(dep, "web", 3)
			a.ExpiresAt = timestamppb.New(f.now.Add(-time.Second))
			return dep, a
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_UNSPECIFIED},
		"below the mark": {func(f *pbFixture, dep string) (string, *cloudpbv2.DeviceAssignment) {
			return dep, f.assignment(dep, "web", 1)
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_UNSPECIFIED},
		"conflicting copy": {func(f *pbFixture, dep string) (string, *cloudpbv2.DeviceAssignment) {
			return dep, f.assignment(dep, "web", 2)
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_UNSPECIFIED},
		"newer revision, bad configuration": {func(f *pbFixture, dep string) (string, *cloudpbv2.DeviceAssignment) {
			a := f.assignment(dep, "web", 3)
			a.ConfigJson = "[]"
			return dep, a
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED},
		"newer revision, one instance": {func(f *pbFixture, _ string) (string, *cloudpbv2.DeviceAssignment) {
			other := uuid.NewString()
			return other, f.assignment(other, "web", 1)
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_TARGET},
		"newer revision, move": {func(f *pbFixture, dep string) (string, *cloudpbv2.DeviceAssignment) {
			other := uuid.NewString()
			a := f.assignment(other, "web", 1)
			a.ReplacesDeploymentId, a.ReplacesRevision = dep, 2 // at the held assignment's mark
			return other, a
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE},
	} {
		f := newPBFixture(t)
		r := f.release(t)
		r.assign.Revision, r.assign.ConfigJson = 2, "{}"
		rec := f.withArtifacts(&fakeArtifacts{r: r})
		dep := r.assign.GetDeploymentId()
		f.assignEntry(r.assign)
		f.cycle()
		f.p.waitPulls()
		held, before := *f.held(dep), f.outcome("web")
		target, a := tc.statement(f, dep)
		f.statement(assignNew, target, a, 0)
		f.cycle()
		f.p.waitPulls()
		o := f.outcome("web")
		switch {
		case tc.reported == cloudpbv2.ApplyOutcome_APPLY_OUTCOME_UNSPECIFIED && o != before:
			t.Errorf("%s: outcome %+v, want the held deployment's %+v kept", name, o, before)
		case tc.reported != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_UNSPECIFIED && (o.EntryID != 2 || o.Outcome != tc.reported):
			t.Errorf("%s: outcome %+v, want %v reported for entry 2", name, o, tc.reported)
		}
		if h := f.held(dep); h == nil || h.EntryID != held.EntryID || f.mark(dep).Revision != 2 || len(rec.imported) != 1 || len(rec.removed) != 1 {
			t.Errorf("%s: held %+v, mark %+v, imports %d, removals %v: want the running deployment untouched", name, h, f.mark(dep), len(rec.imported), rec.removed)
		}
	}
}
