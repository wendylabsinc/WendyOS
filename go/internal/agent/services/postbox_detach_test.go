package services

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

var detachOp = cloudpbv2.DeploymentService_DetachDevice_FullMethodName

// detachRequest is a valid detach of dep (app) for this device; mutate edits it.
func (f *pbFixture) detachRequest(dep, app string) *cloudpbv2.DetachDeviceRequest {
	return &cloudpbv2.DetachDeviceRequest{OrganizationId: f.tenant, DeploymentId: dep, Device: f.uri, AppId: app,
		ValidUntil: timestamppb.New(f.now.Add(time.Hour))}
}

// detach signs msg under method for resource ("" = dep's own) and queues it.
func (f *pbFixture) detach(method string, msg proto.Message, dep, resource string) {
	f.t.Helper()
	if resource == "" {
		resource = "org/" + f.tenant + "/deployment/" + dep + "/device/" + f.uuid
	}
	jws, payload, typ := f.sign(method, msg, resource, []*x509.Certificate{f.opLeaf, f.opCA}, f.opKey, nil)
	f.cloud.add(jws, payload, typ)
}

// A detach of a running deployment keeps the app exactly as it runs and
// ends the deployment: maximum removal mark, held statement (and its
// wrapped deployment key) gone from disk, only unused images pruned, the
// entry reported APPLIED. A later terminal removal is a duplicate that
// deletes nothing, and a queued removal of the app only prunes.
func TestPostboxDetachKeepsTheAppAndEndsTheDeployment(t *testing.T) {
	f, rec, a := runningRelease(t)
	dep := a.GetDeploymentId()
	before, starts := *rec.app, len(rec.starts)
	rec.removed = nil
	f.detach(detachOp, f.detachRequest(dep, "web"), dep, "")
	f.cycle()

	if m := f.mark(dep); f.held(dep) != nil || m == nil || !m.Removed || m.Revision != maxRevision {
		t.Fatalf("held %+v, mark %+v: want the deployment ended at the maximum mark", f.held(dep), m)
	}
	if o := f.outcome("web"); o.EntryID != 2 || o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
		t.Fatalf("outcome %+v: want the detach reported applied on the app's row", o)
	}
	if !reflect.DeepEqual(*rec.app, before) || len(rec.starts) != starts ||
		!slices.EqualFunc(rec.removed, []removal{{app: "web", prune: true}}, removal.equal) {
		t.Fatalf("app %+v (was %+v), starts %d, removed %v: want the container untouched and only unused images pruned",
			*rec.app, before, len(rec.starts)-starts, rec.removed)
	}
	raw, err := os.ReadFile(filepath.Join(f.dir, postboxStateFile))
	if err != nil {
		t.Fatal(err)
	}
	var disk postboxState
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	if len(disk.Assignments) != 0 || !slices.Equal(disk.Detached, []string{"web"}) || len(disk.Removals) != 0 {
		t.Fatalf("on disk: assignments %v, detached %v, removals %v: want no key material left and the app detached",
			disk.Assignments, disk.Detached, disk.Removals)
	}

	rec.removed = nil
	f.statement(unassignNew, dep, nil, maxRevision)
	f.cycle()
	if len(rec.removed) != 0 || rec.app == nil || f.outcome("web").EntryID != 2 {
		t.Fatalf("removed %v, outcome %+v: want the later terminal removal consumed without effect", rec.removed, f.outcome("web"))
	}
	if err := f.p.removeReleases(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(rec.removed, []removal{{app: "web", prune: true}}, removal.equal) {
		t.Fatalf("removed %v: a queued removal of a detached app must only prune", rec.removed)
	}
}

// A terminal removal applied first wins: a later detach is a duplicate.
// A detach of a deployment the device does not hold marks it all the same,
// so nothing can re-attach it and a move into it is refused.
func TestPostboxFirstOfRemovalAndDetachWins(t *testing.T) {
	f, rec, a := runningRelease(t)
	dep := a.GetDeploymentId()
	f.statement(unassignNew, dep, nil, maxRevision)
	f.cycle()
	removed := len(rec.removed)
	f.detach(detachOp, f.detachRequest(dep, "web"), dep, "")
	f.cycle()
	if len(rec.removed) != removed || len(f.p.snapshot().Detached) != 0 || f.outcome("web").EntryID != 2 {
		t.Fatalf("removed %v, detached %v, outcome %+v: want the later detach consumed without effect",
			rec.removed, f.p.snapshot().Detached, f.outcome("web"))
	}

	g := newPBFixture(t)
	gone, dep2 := uuid.NewString(), uuid.NewString()
	g.detach(detachOp, g.detachRequest(gone, "web"), gone, "")
	g.cycle()
	if m := g.mark(gone); m == nil || !m.Removed || m.Revision != maxRevision || len(g.p.snapshot().Detached) != 0 || len(g.p.Outcomes()) != 0 {
		t.Fatalf("mark %+v, outcomes %v: want the maximum mark recorded, audit only", m, g.p.Outcomes())
	}
	move := g.assignment(dep2, "web", 1)
	move.ReplacesDeploymentId, move.ReplacesRevision = gone, 3
	g.statement(assignNew, gone, g.assignment(gone, "web", 5), 0)
	g.statement(assignNew, dep2, move, 0)
	g.cycle()
	if g.held(gone) != nil || g.held(dep2) != nil {
		t.Fatalf("held %v: want an assignment to and a move from the detached deployment refused", g.p.snapshot().Assignments)
	}
}

// Nothing is acted on unless every check passes: payload type, the device
// in the payload, the resource, the deployment's app, and validity.
func TestPostboxDetachRefusals(t *testing.T) {
	f := newPBFixture(t)
	dep := uuid.NewString()
	f.statement(assignNew, dep, f.assignment(dep, "web", 1), 0)
	f.cycle()
	other := "spiffe://wendy.sh/tenant/" + f.tenant + "/device/" + uuid.NewString()

	f.detach(detachOp, &cloudpbv2.UnassignDeviceRequest{OrganizationId: f.tenant, DeploymentId: dep, Device: f.uri,
		ValidUntil: timestamppb.New(f.now.Add(time.Hour)), Revision: maxRevision}, dep, "") // operation
	f.detach(unassignNew, f.detachRequest(dep, "web"), dep, "") // operation
	r := f.detachRequest(dep, "web")
	r.Device = other
	f.detach(detachOp, r, dep, "")                                                           // target
	f.detach(detachOp, f.detachRequest(dep, "web"), dep, "org/"+f.tenant+"/deployment/"+dep) // target
	f.detach(detachOp, f.detachRequest(dep, "db"), dep, "")                                  // app
	r = f.detachRequest(dep, "web")
	r.ValidUntil = timestamppb.New(f.now.Add(-time.Second))
	f.detach(detachOp, r, dep, "") // stale
	r = f.detachRequest(dep, "web")
	r.ValidUntil = nil
	f.detach(detachOp, r, dep, "") // validity_missing
	r = f.detachRequest(dep, "web")
	r.ValidUntil = timestamppb.New(f.now.Add(postboxPlatformCap + time.Hour))
	f.detach(detachOp, r, dep, "") // above_platform_cap
	f.cycle()

	want := map[string]int64{"operation": 2, "target": 2, "app": 1, "stale": 1, "validity_missing": 1, "above_platform_cap": 1}
	for reason, n := range want {
		if f.p.refusals[reason] != n {
			t.Errorf("refusals[%q] = %d, want %d (all: %v)", reason, f.p.refusals[reason], n, f.p.refusals)
		}
	}
	if f.held(dep) == nil || f.mark(dep).Removed || len(f.p.snapshot().Detached) != 0 {
		t.Fatalf("held %+v, mark %+v: want every refused detach to change nothing", f.held(dep), f.mark(dep))
	}
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_TARGET || o.EntryID != 6 {
		t.Fatalf("outcome %+v: want the wrong-app detach reported on the held app (entry 6)", o)
	}
}

// A pull in progress stops: nothing it would import stays, and only unused
// images are pruned.
func TestPostboxDetachStopsAPullInProgress(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	arts := &fakeArtifacts{r: r, materialErr: func(int) error { return status.Error(codes.Unavailable, "registry unreachable") }}
	rec := f.withArtifacts(arts)
	f.p.retry = time.Hour // the pull only ends by cancellation
	f.assignEntry(r.assign)
	f.cycleWithin(5 * time.Second)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		arts.mu.Lock()
		calls := arts.materialCalls
		arts.mu.Unlock()
		if calls > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pull never called the registry")
		}
	}
	dep := r.assign.GetDeploymentId()
	f.detach(detachOp, f.detachRequest(dep, "web"), dep, "")
	f.cycleWithin(5 * time.Second)
	f.p.mu.Lock()
	running := len(f.p.pulls)
	f.p.mu.Unlock()
	if running != 0 || len(rec.imported) != 0 || f.held(dep) != nil ||
		!slices.EqualFunc(rec.removed, []removal{{app: "web", prune: true}}, removal.equal) {
		t.Fatalf("pulls %d, imported %d, held %+v, removed %v: want the pull stopped and nothing left", running, len(rec.imported), f.held(dep), rec.removed)
	}
	if o := f.outcome("web"); o.EntryID != 2 || o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
		t.Fatalf("outcome %+v: want the detach applied", o)
	}
}

// One instance per app: a detached deployment is not held, so a new
// deployment of the app is accepted and replaces the direct run; after
// that, removing it removes the app as usual.
func TestPostboxAssignAfterDetachReplacesTheDirectRun(t *testing.T) {
	f, rec, a := runningRelease(t)
	dep := a.GetDeploymentId()
	f.detach(detachOp, f.detachRequest(dep, "web"), dep, "")
	f.cycle()
	next := proto.Clone(a).(*cloudpbv2.DeviceAssignment)
	next.DeploymentId, next.Revision = uuid.NewString(), 1
	f.assignEntry(next)
	f.cycle()
	f.p.waitPulls()
	if f.held(next.GetDeploymentId()) == nil || f.p.refusals["one_instance"] != 0 || len(f.p.snapshot().Detached) != 0 {
		t.Fatalf("held %v, refusals %v, detached %v: want the new deployment accepted and the direct run replaced",
			f.p.snapshot().Assignments, f.p.refusals, f.p.snapshot().Detached)
	}
	rec.removed = nil
	f.unassignEntry(next.GetDeploymentId())
	f.cycle()
	if !slices.EqualFunc(rec.removed, []removal{{app: "web"}}, removal.equal) {
		t.Fatalf("removed %v: want the replacing deployment's removal to remove the app", rec.removed)
	}
}

// A detach whose prune a crash interrupted is finished on restart: the app
// stays and is still a direct run.
func TestPostboxDetachCrashAfterCommit(t *testing.T) {
	f, rec, a := runningRelease(t)
	dep := a.GetDeploymentId()
	f.detach(detachOp, f.detachRequest(dep, "web"), dep, "")
	snap := f.committedWrite()
	rec.removed = nil
	f.restartFrom(snap)
	if o := f.outcome("web"); o.EntryID != 2 || o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED ||
		!slices.EqualFunc(rec.removed, []removal{{app: "web", prune: true}}, removal.equal) || rec.app == nil {
		t.Fatalf("outcome %+v, removed %v: want the detach finished as a prune", o, rec.removed)
	}
}

// Every pull says which relayed operations this agent accepts, DetachDevice
// among them.
func TestPostboxSendsAcceptedOperations(t *testing.T) {
	f := newPBFixture(t)
	var mu sync.Mutex
	var got [][]string
	f.cloud.onPull = func(in *cloudpbv2.PullChangesRequest) {
		mu.Lock()
		got = append(got, in.GetAcceptedOperations())
		mu.Unlock()
	}
	f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
	f.cycle()
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no pull")
	}
	for _, ops := range got {
		if !slices.Equal(ops, postboxAccepted) || !slices.Contains(ops, "wendycloud.v2.DeploymentService/DetachDevice") {
			t.Fatalf("accepted_operations %v, want %v with DetachDevice", ops, postboxAccepted)
		}
	}
}
