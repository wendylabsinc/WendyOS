package services

import (
	"context"
	"crypto/x509"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func (f *pbFixture) unassignEntry(dep string) {
	resource := "org/" + f.tenant + "/deployment/" + dep + "/device/" + f.uuid
	req := &cloudpbv2.UnassignDeviceRequest{OrganizationId: f.tenant, DeploymentId: dep, Device: f.uri, ValidUntil: timestamppb.New(f.now.Add(time.Hour))}
	jws, payload, typ := f.sign(cloudpbv2.ArtifactRegistryService_UnassignDevice_FullMethodName, req, resource, []*x509.Certificate{f.opLeaf, f.opCA}, f.opKey, nil)
	f.cloud.add(jws, payload, typ)
}

// cycleWithin runs one cycle without waiting for the pulls it starts, and
// fails if the cycle itself does not return in time.
func (f *pbFixture) cycleWithin(d time.Duration) {
	f.t.Helper()
	done := make(chan error, 1)
	go func() { done <- f.p.cycle(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			f.t.Fatal(err)
		}
	case <-time.After(d):
		f.t.Fatalf("the cycle did not return within %v: a pull is holding the command queue", d)
	}
}

// B1: a pull waiting on an unreachable registry holds no other command: a
// later stop for another app is applied and acked, and the deployment's
// removal cancels the pull and removes its release.
func TestRegistryPullRunsBesideTheCommandQueue(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	arts := &fakeArtifacts{r: r, materialErr: func(int) error {
		return status.Error(codes.Unavailable, "the assignment check could not be made; retry")
	}}
	rec := f.withArtifacts(arts)
	f.ctr.resolvedIDs["other"] = []string{"other"}
	f.assignEntry(r.assign)
	f.control("other", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
	f.cycleWithin(5 * time.Second)

	if !slices.Equal(f.ctr.stopCalls, []string{"other"}) || f.outcome("other").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
		t.Fatalf("stop calls %v, outcome %v: want the stop behind the pull applied", f.ctr.stopCalls, f.outcome("other").Outcome)
	}
	if st := f.p.snapshot(); st.HighWater != 2 || st.Acked != 2 {
		t.Fatalf("high water %d, acked %d: want both entries verified and acked", st.HighWater, st.Acked)
	}
	if o := f.outcome("web"); o.EntryID != 1 || o.Outcome != outcomePulling {
		t.Fatalf("web outcome = %+v, want entry 1 still pulling", o)
	}

	f.unassignEntry(r.assign.GetDeploymentId())
	f.cycleWithin(5 * time.Second)
	f.p.mu.Lock()
	running := len(f.p.pulls)
	f.p.mu.Unlock()
	if running != 0 || len(f.p.snapshot().Assignments) != 0 {
		t.Fatalf("pulls %d, assignments %v: want the removal to cancel the pull", running, f.p.snapshot().Assignments)
	}
	if len(rec.imported) != 0 || !slices.EqualFunc(rec.removed, []removal{{app: "web"}}, removal.equal) {
		t.Fatalf("imported %d, removed %v: want nothing imported and the app's releases removed", len(rec.imported), rec.removed)
	}
	if o := f.outcome("web"); o.EntryID != 3 || o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
		t.Fatalf("web outcome = %+v, want the removal applied", o)
	}
}

// B2: a pull the agent's stop interrupted is resumed from the held
// deployment after the restart; a finished pull is not repeated.
func TestRegistryPullResumesAfterARestart(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	arts := &fakeArtifacts{r: r, materialErr: func(call int) error {
		if call == 1 {
			stop() // the agent stops mid-pull
			return status.Error(codes.Unavailable, "connection closed")
		}
		return nil
	}}
	rec := f.withArtifacts(arts)
	f.assignEntry(r.assign)
	_ = f.p.cycle(ctx)
	f.p.waitPulls()
	if len(rec.imported) != 0 {
		t.Fatalf("imported %d before the restart", len(rec.imported))
	}

	f.p = f.newPostbox() // restart
	rec = f.withArtifacts(arts)
	f.cycle()
	if len(rec.imported) != 1 || f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED || f.outcome("web").EntryID != 1 {
		t.Fatalf("after restart: imported %d, outcome %+v: want the pull resumed and applied for entry 1", len(rec.imported), f.outcome("web"))
	}

	f.p = f.newPostbox() // another restart
	rec = f.withArtifacts(arts)
	f.cycle()
	if len(rec.imported) != 0 || arts.materialCalls != 2 {
		t.Fatalf("imported %d, material calls %d: a finished pull ran again", len(rec.imported), arts.materialCalls)
	}
}

// B5: a release meant to run is on the device but nothing starts it yet, so
// it is not reported as applied, nor as a failure: it carries no outcome
// until the not-started value is accepted, and is not counted as a refusal.
func TestRegistryPullRunningReleaseIsNotApplied(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	r.assign.DesiredState = cloudpbv2.Running_RUNNING
	rec := f.withArtifacts(&fakeArtifacts{r: r})
	f.assignEntry(r.assign)
	f.cycle()
	o := f.outcome("web")
	if o.Outcome != outcomeNotStarted || o.Outcome == cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED ||
		o.Outcome == cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED || o.EntryID != 1 || len(rec.imported) != 1 {
		t.Fatalf("outcome %+v, imported %d: want the release imported, neither applied nor failed", o, len(rec.imported))
	}
	if len(f.p.refusals) != 0 {
		t.Fatalf("refusals = %v, want none for a release not started yet", f.p.refusals)
	}
	if a := f.p.snapshot().Assignments[r.assign.GetDeploymentId()]; a == nil || !a.Pulled {
		t.Fatalf("assignment not marked pulled: want the pull finished, not resumed")
	}
}

// Install only (desired state stopped) is applied once the image is imported.
func TestRegistryPullInstallOnlyIsAppliedOnceImported(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	r.assign.DesiredState = cloudpbv2.Running_STOPPED
	rec := f.withArtifacts(&fakeArtifacts{r: r})
	f.assignEntry(r.assign)
	f.cycle()
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED || len(rec.imported) != 1 {
		t.Fatalf("outcome %v, imported %d: want APPLIED", o.Outcome, len(rec.imported))
	}
}

// B10: an imported release supersedes the app's other releases; removing the
// deployment removes the app's releases except one another held deployment
// of the app still uses.
func TestRegistryPullRemovesSupersededAndRemovedReleases(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	rec := f.withArtifacts(&fakeArtifacts{r: r})
	f.assignEntry(r.assign)
	f.cycle()
	rel := r.assign.GetAppReleaseId()
	if !slices.EqualFunc(rec.removed, []removal{{app: "web", keep: []string{rel}}}, removal.equal) {
		t.Fatalf("removed %v after the import, want the app's other releases", rec.removed)
	}

	// The device moves to another deployment of the app before the first one
	// is removed.
	other := uuid.NewString()
	f.p.mu.Lock()
	f.p.state.Assignments[other] = &postboxAssignment{AppID: "web", AppReleaseID: "other-release", Pulled: true}
	f.p.mu.Unlock()
	rec.removed = nil
	f.unassignEntry(r.assign.GetDeploymentId())
	f.cycle()
	if !slices.EqualFunc(rec.removed, []removal{{app: "web", keep: []string{"other-release"}}}, removal.equal) {
		t.Fatalf("removed %v, want web's releases except the other deployment's", rec.removed)
	}
}

// A failed removal is reported and retried on the next cycle.
func TestRegistryPullRetriesAFailedRemoval(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	rec := f.withArtifacts(&fakeArtifacts{r: r})
	f.assignEntry(r.assign)
	f.cycle()
	rec.removeErr = errors.New("containerd unavailable")
	rec.removed = nil
	f.unassignEntry(r.assign.GetDeploymentId())
	f.cycle()
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED || !slices.Equal(f.p.snapshot().Removals, []string{"web"}) {
		t.Fatalf("outcome %v, removals %v: want the failure reported and the removal kept", o.Outcome, f.p.snapshot().Removals)
	}
	rec.removeErr = nil
	f.cycle()
	if len(rec.removed) != 2 || len(f.p.snapshot().Removals) != 0 {
		t.Fatalf("removed %v, removals %v: want the removal retried and cleared", rec.removed, f.p.snapshot().Removals)
	}
}

// Reset removes every release and stops every pull.
func TestPostboxResetRemovesEveryRelease(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	rec := f.withArtifacts(&fakeArtifacts{r: r, materialErr: func(int) error {
		return status.Error(codes.Unavailable, "registry unreachable")
	}})
	f.assignEntry(r.assign)
	f.cycleWithin(5 * time.Second)
	if err := f.p.RemoveAllReleases(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.p.mu.Lock()
	running := len(f.p.pulls)
	f.p.mu.Unlock()
	if running != 0 || !slices.EqualFunc(rec.removed, []removal{{}}, removal.equal) {
		t.Fatalf("pulls %d, removed %v: want every pull stopped and every release removed", running, rec.removed)
	}
}

// releaseRemoverMock is a containerd client that holds registry releases.
type releaseRemoverMock struct {
	*mockContainerdClient
	apps  []string
	steps []string
}

func (m *releaseRemoverMock) ReleaseApps(context.Context, string, []string) ([]string, error) {
	return m.apps, nil
}

func (m *releaseRemoverMock) DeleteContainer(ctx context.Context, name string, deleteImage bool) error {
	m.steps = append(m.steps, "delete app "+name)
	return m.mockContainerdClient.DeleteContainer(ctx, name, deleteImage)
}

func (m *releaseRemoverMock) DeleteReleaseImages(_ context.Context, app string, keep []string) error {
	m.steps = append(m.steps, "delete releases of "+app)
	return nil
}

// The app running a removed release is deleted through the CLI's delete
// path before its release images go.
func TestPostboxControlRemovesTheAppThenItsReleases(t *testing.T) {
	m := &releaseRemoverMock{mockContainerdClient: &mockContainerdClient{resolvedIDs: map[string][]string{"web": {"web"}}}, apps: []string{"web"}}
	control := NewPostboxControl(NewContainerService(zap.NewNop(), m))
	if err := control.RemoveReleases(context.Background(), "web", nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{"delete app web", "delete releases of web"}; !slices.Equal(m.steps, want) {
		t.Fatalf("steps %v, want %v", m.steps, want)
	}
}

// A newer statement for the same deployment cancels the running pull before
// its own starts: only the newer one imports.
func TestRegistryPullReplacementCancelsThePull(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	var mu sync.Mutex
	down := true
	arts := &fakeArtifacts{r: r, materialErr: func(int) error {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return status.Error(codes.Unavailable, "registry unreachable")
		}
		return nil
	}}
	rec := f.withArtifacts(arts)
	f.p.retry = time.Hour // after its first failure the old pull only ends by cancellation
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
			t.Fatal("the first pull never called the registry")
		}
	}
	mu.Lock()
	down = false
	mu.Unlock()
	f.assignEntry(r.assign) // the same deployment again: a new revision
	f.cycle()
	if len(rec.imported) != 1 || f.outcome("web").EntryID != 2 || f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
		t.Fatalf("imported %d, outcome %+v: want only the newer statement's pull, applied", len(rec.imported), f.outcome("web"))
	}
	if a := f.p.snapshot().Assignments[r.assign.GetDeploymentId()]; a == nil || a.EntryID != 2 || !a.Pulled {
		t.Fatalf("held assignment not entry 2 pulled")
	}
}

// A pull resumed after the statement expired fetches nothing.
func TestRegistryPullResumedAfterExpiryIsRefused(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	arts := &fakeArtifacts{r: r, materialErr: func(call int) error {
		if call == 1 {
			stop()
			return status.Error(codes.Unavailable, "connection closed")
		}
		return nil
	}}
	f.withArtifacts(arts)
	f.assignEntry(r.assign)
	_ = f.p.cycle(ctx)
	f.p.waitPulls()

	f.now = f.now.Add(2 * time.Hour) // the device comes back after expires_at
	f.p = f.newPostbox()
	rec := f.withArtifacts(arts)
	f.cycle()
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULL_REFUSED || arts.materialCalls != 1 || len(rec.imported) != 0 {
		t.Fatalf("outcome %v, material calls %d, imported %d: want PULL_REFUSED with nothing fetched", o.Outcome, arts.materialCalls, len(rec.imported))
	}
}

// An ABSENT revision of a held deployment removes the app's releases,
// its own included.
func TestRegistryPullAbsentRevisionRemovesTheRelease(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	rec := f.withArtifacts(&fakeArtifacts{r: r})
	f.assignEntry(r.assign)
	f.cycle()
	absent := proto.Clone(r.assign).(*cloudpbv2.DeviceAssignment)
	absent.DesiredState = cloudpbv2.Running_ABSENT
	rec.removed = nil
	f.assignEntry(absent)
	f.cycle()
	if !slices.EqualFunc(rec.removed, []removal{{app: "web"}}, removal.equal) || f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
		t.Fatalf("removed %v, outcome %v: want every web release removed and the revision applied", rec.removed, f.outcome("web").Outcome)
	}
}

// A pull whose outcome could not be saved holds until it lands, and is not
// pulled again.
func TestRegistryPullRetriesAFailedOutcomeSave(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	arts := &fakeArtifacts{r: r}
	rec := f.withArtifacts(arts)
	var mu sync.Mutex
	failed := false
	write := f.p.write
	f.p.write = func(dir, name string, data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if !failed && strings.Contains(string(data), `"pulled":true`) {
			failed = true
			return errors.New("disk full")
		}
		return write(dir, name, data)
	}
	f.assignEntry(r.assign)
	f.cycle()
	f.cycle()
	a := f.p.snapshot().Assignments[r.assign.GetDeploymentId()]
	if !failed || a == nil || !a.Pulled || arts.materialCalls != 1 || len(rec.imported) != 1 {
		t.Fatalf("save failed %v, pulled %v, material calls %d, imports %d: want one pull, recorded after the retry", failed, a != nil && a.Pulled, arts.materialCalls, len(rec.imported))
	}
}

// decide never replaces an outcome a finished pull recorded for the same
// entry ("pulling" saved after the pull ended); a newer entry does replace it.
func TestPostboxDecideKeepsAFinishedPullsOutcome(t *testing.T) {
	f := newPBFixture(t)
	f.cycle() // loads the state
	if err := f.p.commit(func(st *postboxState) {
		st.Outcomes = map[string]*postboxOutcome{"web": {EntryID: 1, Outcome: cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED}}
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.p.decide(context.Background(), &cloudpbv2.PostboxEntry{EntryId: 1}, "c1", "web", outcomePulling, "", nil); err != nil {
		t.Fatal(err)
	}
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
		t.Fatalf("outcome %v, want the finished pull's APPLIED kept", o.Outcome)
	}
	if err := f.p.decide(context.Background(), &cloudpbv2.PostboxEntry{EntryId: 2}, "c2", "web", cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "apply", errors.New("x")); err != nil {
		t.Fatal(err)
	}
	if o := f.outcome("web"); o.EntryID != 2 {
		t.Fatalf("outcome %+v, want entry 2", o)
	}
}

// finishPull and recordPullState write nothing for a replaced or removed
// statement, and never over a newer entry's outcome.
func TestPostboxPullRecordsOnlyForItsOwnEntry(t *testing.T) {
	f := newPBFixture(t)
	f.cycle()
	a := &cloudpbv2.DeviceAssignment{DeploymentId: "d", AppId: "web"}
	set := func(heldEntry, outcomeEntry int64) {
		if err := f.p.commit(func(st *postboxState) {
			st.Assignments = map[string]*postboxAssignment{"d": {AppID: "web", EntryID: heldEntry}}
			st.Outcomes = map[string]*postboxOutcome{"web": {EntryID: outcomeEntry, Outcome: outcomePulling}}
		}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	applied := cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED

	set(2, 2) // replaced by entry 2
	f.p.finishPull(ctx, a, 1, "c1", applied, "", nil)
	if st := f.p.snapshot(); st.Assignments["d"].Pulled || st.Outcomes["web"].EntryID != 2 {
		t.Fatalf("replaced: %+v %+v", st.Assignments["d"], st.Outcomes["web"])
	}
	set(1, 3) // a newer entry decided the app
	f.p.finishPull(ctx, a, 1, "c1", applied, "", nil)
	if st := f.p.snapshot(); !st.Assignments["d"].Pulled || st.Outcomes["web"].EntryID != 3 {
		t.Fatalf("newer entry: %+v %+v", st.Assignments["d"], st.Outcomes["web"])
	}
	set(1, 1)
	f.p.finishPull(ctx, a, 1, "c1", applied, "", nil)
	if st := f.p.snapshot(); !st.Assignments["d"].Pulled || st.Outcomes["web"].Outcome != applied {
		t.Fatalf("own entry: %+v %+v", st.Assignments["d"], st.Outcomes["web"])
	}

	// A transient state is recorded for its own entry only.
	set(1, 1)
	f.p.recordPullState(a, 1, "c1", cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULL_REFUSED)
	if o := f.p.snapshot().Outcomes["web"]; o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULL_REFUSED {
		t.Fatalf("own entry state: %+v", o)
	}
	set(2, 2)
	f.p.recordPullState(a, 1, "c1", cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULL_REFUSED)
	if o := f.p.snapshot().Outcomes["web"]; o.Outcome != outcomePulling {
		t.Fatalf("replaced entry state: %+v", o)
	}
}
