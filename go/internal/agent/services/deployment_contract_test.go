package services

import (
	"crypto/x509"
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

// Removal marks are kept for at least a year and then dropped.
func TestRemovalMarksArePrunedAfterTheyCanMatter(t *testing.T) {
	now := time.Now()
	st := &postboxState{Marks: map[string]*postboxMark{
		"recent": {Revision: 2, Removed: true, RemovedAt: now.Add(-366 * 24 * time.Hour).Unix()},
		"old":    {Revision: 2, Removed: true, RemovedAt: now.Add(-removalMarkKeep - time.Hour).Unix()},
		"live":   {Revision: 1},
	}}
	markRemoved(st, "new", 3, now)
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
