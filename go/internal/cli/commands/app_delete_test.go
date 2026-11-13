package commands

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func (f *fakeRegistry) GetApp(_ context.Context, in *cloudpb.GetAppRequest) (*cloudpb.App, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := slices.IndexFunc(f.apps, func(a *cloudpb.App) bool { return a.GetId() == in.GetId() }); i >= 0 {
		return f.apps[i], nil
	}
	return nil, status.Error(codes.NotFound, "app not found")
}

// DeleteApp is cloud's by-app delete: each entry passes its own gate (a
// removal at the maximum revision, never for a device the caller cannot
// reach), covered devices leave the pending list, and the call after which
// none is left deletes the app.
func (f *fakeRegistry) DeleteApp(_ context.Context, in *cloudpb.SignedRequest) (*cloudpb.DeleteAppResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req cloudpb.DeleteAppRequest
	if err := f.accept(cloudpb.AppService_DeleteApp_FullMethodName, in, &req); err != nil {
		return nil, err
	}
	kind := map[string]cloudpb.PendingDeviceStatement_Kind{}
	for _, r := range f.pending[req.GetId()] {
		kind[r.GetDevice()] = r.GetKind()
	}
	covered := map[string]bool{}
	for i, e := range req.GetDeviceRemovals() {
		var u cloudpb.UnassignDeviceRequest
		if err := f.accept(cloudpb.DeploymentService_UnassignDevice_FullMethodName, e, &u); err != nil {
			return nil, err
		}
		if u.GetRevision() != maxRevision {
			return nil, status.Errorf(codes.InvalidArgument, "Removal %d: Deleting an app takes final removals.", i)
		}
		if kind[u.GetDevice()] == cloudpb.PendingDeviceStatement_KIND_DETACH {
			return nil, status.Errorf(codes.PermissionDenied, "Removal %d (device %s) was refused: You can't reach this device. Ask someone who can to remove it, or detach it (the app keeps running there)", i, u.GetDevice())
		}
		f.removed = append(f.removed, &u)
		covered[u.GetDevice()] = true
	}
	for _, e := range req.GetDeviceDetachments() {
		var d cloudpb.DetachDeviceRequest
		if err := f.accept(cloudpb.DeploymentService_DetachDevice_FullMethodName, e, &d); err != nil {
			return nil, err
		}
		f.detached = append(f.detached, &d)
		covered[d.GetDevice()] = true
	}
	var left []*cloudpb.PendingDeviceStatement
	for _, r := range f.pending[req.GetId()] {
		if !covered[r.GetDevice()] {
			left = append(left, r)
		}
	}
	f.pending[req.GetId()] = left
	if len(left) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "%d devices still need a removal or a detach", len(left))
	}
	f.apps = slices.DeleteFunc(f.apps, func(a *cloudpb.App) bool { return a.GetId() == req.GetId() })
	return &cloudpb.DeleteAppResponse{Success: true}, nil
}

const exampleApp = "sh.wendy.example"

// deleteFixture is line-3 deployed to packer-01, with cloud listing rows
// for the delete of the example app.
func deleteFixture(t *testing.T, rows ...*cloudpb.PendingDeviceStatement) (*registryOp, *fakeRegistry, string) {
	t.Helper()
	op, fake, _, depID := deployedFixture(t)
	for _, r := range rows {
		if r.DeploymentId == "" {
			r.DeploymentId = depID
		}
	}
	fake.pending[exampleApp] = rows
	return op, fake, depID
}

func row(kind cloudpb.PendingDeviceStatement_Kind, device, name string) *cloudpb.PendingDeviceStatement {
	return &cloudpb.PendingDeviceStatement{Kind: kind, Device: deviceURI(testOperatorTenant, device), DeviceDisplayName: name}
}

const (
	kindUnassign = cloudpb.PendingDeviceStatement_KIND_UNASSIGN
	kindDetach   = cloudpb.PendingDeviceStatement_KIND_DETACH
)

// Removal is the default: a terminal removal at the maximum revision, bound
// to the deployment and device, x5c only, valid for the platform cap.
func TestAppDeleteRemovesByDefault(t *testing.T) {
	op, fake, depID := deleteFixture(t, row(kindUnassign, vectorDevice, "packer-01"))
	var out bytes.Buffer
	if err := runAppDelete(context.Background(), op, exampleApp, nil, true, &out, false); err != nil {
		t.Fatal(err)
	}
	calls := fake.signed(cloudpb.DeploymentService_UnassignDevice_FullMethodName)
	got := calls[len(calls)-1]
	u := got.payload.(*cloudpb.UnassignDeviceRequest)
	if len(fake.detached) != 0 || u.GetRevision() != maxRevision || got.resource != "org/"+testOperatorTenant+"/deployment/"+depID+"/device/"+vectorDevice || got.header["x5c"] == nil ||
		u.GetValidUntil().AsTime().Before(time.Now().Add(364*24*time.Hour)) {
		t.Fatalf("removal %v resource %s header %v", u, got.resource, got.header)
	}
	if len(fake.apps) != 0 || !strings.Contains(out.String(), exampleApp+" is deleted.") {
		t.Fatalf("not deleted:\n%s", out.String())
	}
}

// A device is detached only when --detach names it or the operator says so
// for that device; every other device is removed.
func TestAppDeleteDetachesOnlyChosenDevices(t *testing.T) {
	rows := func() []*cloudpb.PendingDeviceStatement {
		return []*cloudpb.PendingDeviceStatement{row(kindUnassign, vectorDevice, "packer-01"), row(kindUnassign, vectorOtherDevice, "dock-cam-07")}
	}
	op, fake, _ := deleteFixture(t, rows()...)
	var out bytes.Buffer
	if err := runAppDelete(context.Background(), op, exampleApp, []string{"packer-01"}, true, &out, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.detached) != 1 || fake.detached[0].GetDevice() != deviceURI(testOperatorTenant, vectorDevice) || fake.detached[0].GetAppId() != exampleApp ||
		fake.removed[len(fake.removed)-1].GetDevice() != deviceURI(testOperatorTenant, vectorOtherDevice) {
		t.Fatalf("detached %v removed %v", fake.detached, fake.removed)
	}
	for _, want := range []string{"packer-01 keeps running sh.wendy.example as a direct run.", "which may hold secrets"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}

	op, fake, _ = deleteFixture(t, rows()...)
	deployConfirm = func(q string) (bool, error) {
		return strings.HasPrefix(q, "Delete ") || strings.HasPrefix(q, "Detach dock-cam-07 instead?"), nil
	}
	deployAsk = func(string, string, string) (string, error) { return exampleApp, nil }
	if err := runAppDelete(context.Background(), op, exampleApp, nil, false, io.Discard, true); err != nil {
		t.Fatal(err)
	}
	if len(fake.detached) != 1 || fake.detached[0].GetDevice() != deviceURI(testOperatorTenant, vectorOtherDevice) {
		t.Fatalf("interactive detached %v", fake.detached)
	}
}

// Without a terminal, a device the operator cannot reach must be named with
// --detach; otherwise nothing is sent.
func TestAppDeleteUnreachableNeedsDetachWithoutTerminal(t *testing.T) {
	op, fake, _ := deleteFixture(t, row(kindDetach, vectorOtherDevice, "dock-cam-07"))
	err := runAppDelete(context.Background(), op, exampleApp, nil, true, io.Discard, false)
	if err == nil || !strings.Contains(err.Error(), "1 device can only be detached. Add --detach <device> for each, or run in a terminal.") ||
		len(fake.signed(cloudpb.AppService_DeleteApp_FullMethodName)) != 0 {
		t.Fatalf("unreachable without --detach: %v", err)
	}
	if err := runAppDelete(context.Background(), op, exampleApp, []string{"dock-cam-07"}, true, io.Discard, false); err != nil || len(fake.detached) != 1 {
		t.Fatalf("named: %v %v", err, fake.detached)
	}
}

// At most 200 devices per call; cloud's "still need" after a batch that is
// not the last one means: send the next.
func TestAppDeleteSendsBatchesOf200(t *testing.T) {
	var rows []*cloudpb.PendingDeviceStatement
	for range 450 {
		rows = append(rows, row(kindUnassign, uuid.NewString(), ""))
	}
	op, fake, _ := deleteFixture(t, rows...)
	if err := runAppDelete(context.Background(), op, exampleApp, nil, true, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	var sizes []int
	for _, c := range fake.signed(cloudpb.AppService_DeleteApp_FullMethodName) {
		sizes = append(sizes, len(c.payload.(*cloudpb.DeleteAppRequest).GetDeviceRemovals()))
	}
	if fmt.Sprint(sizes) != "[200 200 50]" || len(fake.apps) != 0 {
		t.Fatalf("batches %v", sizes)
	}
}

// An app already being deleted cannot be cancelled: no confirmation is
// asked, only each device's choice.
func TestAppDeleteBeingDeletedSkipsConfirmations(t *testing.T) {
	op, fake, _ := deleteFixture(t, row(kindUnassign, vectorDevice, "packer-01"))
	fake.apps[0].DeletionStartedAt = timestamppb.Now()
	var asked []string
	deployConfirm = func(q string) (bool, error) { asked = append(asked, q); return false, nil }
	var out bytes.Buffer
	if err := runAppDelete(context.Background(), op, exampleApp, nil, false, &out, true); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(asked) != "[Detach packer-01 instead? The app keeps running there as a direct run.]" ||
		!strings.Contains(out.String(), "sh.wendy.example is being deleted: 1 device still needs a removal or a detach. Finishing.") || len(fake.apps) != 0 {
		t.Fatalf("asked %v\n%s", asked, out.String())
	}
}

// The outer DeleteApp binds the app; with no device to tell, one call
// deletes it after the first confirmation.
func TestAppDeleteSignsTheApp(t *testing.T) {
	op, fake, _ := deleteFixture(t)
	deployConfirm = func(q string) (bool, error) { return strings.HasPrefix(q, "Delete sh.wendy.example?"), nil }
	if err := runAppDelete(context.Background(), op, exampleApp, nil, false, io.Discard, true); err != nil {
		t.Fatal(err)
	}
	calls := fake.signed(cloudpb.AppService_DeleteApp_FullMethodName)
	if len(calls) != 1 || calls[0].resource != "org/"+testOperatorTenant+"/app/"+exampleApp || len(fake.apps) != 0 {
		t.Fatalf("DeleteApp %+v", calls)
	}
}
