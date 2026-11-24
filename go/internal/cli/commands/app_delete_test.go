package commands

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// DeleteApp is cloud's by-app delete: each entry passes its own gate (a
// removal at the maximum revision, never for a device the caller cannot
// reach), covered devices leave the pending list, and the call after which
// none is left deletes the app.
func (f fakeApps) DeleteApp(_ context.Context, in *cloudpb.SignedRequest) (*cloudpb.DeleteAppResponse, error) {
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
	covered := map[[2]string]bool{} // (deployment, device)
	inCall := map[string]bool{}
	once := func(device string) error {
		if inCall[device] {
			return status.Error(codes.InvalidArgument, "This device already has an entry in this call. Send one per device.")
		}
		inCall[device] = true
		return nil
	}
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
		if err := once(u.GetDevice()); err != nil {
			return nil, err
		}
		f.removed = append(f.removed, &u)
		covered[[2]string{u.GetDeploymentId(), u.GetDevice()}] = true
	}
	for _, e := range req.GetDeviceDetachments() {
		var d cloudpb.DetachDeviceRequest
		if err := f.accept(cloudpb.DeploymentService_DetachDevice_FullMethodName, e, &d); err != nil {
			return nil, err
		}
		if err := once(d.GetDevice()); err != nil {
			return nil, err
		}
		f.detached = append(f.detached, &d)
		covered[[2]string{d.GetDeploymentId(), d.GetDevice()}] = true
	}
	var left []*cloudpb.PendingDeviceStatement
	for _, r := range f.pending[req.GetId()] {
		if !covered[[2]string{r.GetDeploymentId(), r.GetDevice()}] {
			left = append(left, r)
		}
	}
	f.pending[req.GetId()] = left
	if len(left) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "%d devices still need a removal or a detach", len(left))
	}
	delete(f.apps, req.GetId())
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
	return &cloudpb.PendingDeviceStatement{Kind: kind, Device: certs.DeviceSPIFFEURI(testOperatorTenant, device), DeviceDisplayName: name}
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
		time.Until(u.GetValidUntil().AsTime()) > cloudrequest.MaxSigned || time.Until(u.GetValidUntil().AsTime()) < cloudrequest.MaxSigned-time.Hour {
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
	if len(fake.detached) != 1 || fake.detached[0].GetDevice() != certs.DeviceSPIFFEURI(testOperatorTenant, vectorDevice) || fake.detached[0].GetAppId() != exampleApp ||
		fake.removed[len(fake.removed)-1].GetDevice() != certs.DeviceSPIFFEURI(testOperatorTenant, vectorOtherDevice) {
		t.Fatalf("detached %v removed %v", fake.detached, fake.removed)
	}
	for _, want := range []string{"packer-01 keeps running sh.wendy.example as a direct run.", "which may hold secrets"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}

	op, fake, _ = deleteFixture(t, rows()...)
	askConfirm = func(q string) (bool, error) {
		return strings.HasPrefix(q, "Delete ") || strings.HasPrefix(q, "Detach dock-cam-07 instead?"), nil
	}
	askText = func(string, string, string, tui.ValidateFunc) (string, error) { return exampleApp, nil }
	if err := runAppDelete(context.Background(), op, exampleApp, nil, false, io.Discard, true); err != nil {
		t.Fatal(err)
	}
	if len(fake.detached) != 1 || fake.detached[0].GetDevice() != certs.DeviceSPIFFEURI(testOperatorTenant, vectorOtherDevice) {
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

// A device in two deployments of the app gets one removal per deployment,
// one per call (cloud refuses a device twice in a call); a pair the list
// repeats is sent once.
func TestAppDeleteDeviceInTwoDeploymentsOnePerCall(t *testing.T) {
	second := row(kindUnassign, vectorDevice, "packer-01")
	second.DeploymentId = uuid.NewString()
	op, fake, depID := deleteFixture(t, row(kindUnassign, vectorDevice, "packer-01"), row(kindUnassign, vectorDevice, "packer-01"), second)
	if err := runAppDelete(context.Background(), op, exampleApp, nil, true, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	var sizes, deps []string
	for _, c := range fake.signed(cloudpb.AppService_DeleteApp_FullMethodName) {
		sizes = append(sizes, fmt.Sprint(len(c.payload.(*cloudpb.DeleteAppRequest).GetDeviceRemovals())))
	}
	for _, u := range fake.removed {
		deps = append(deps, u.GetDeploymentId())
	}
	if fmt.Sprint(sizes) != "[1 1]" || fmt.Sprint(deps) != fmt.Sprint([]string{depID, second.DeploymentId}) || len(fake.apps) != 0 {
		t.Fatalf("removals per call %v, deployments %v, apps %v", sizes, deps, fake.apps)
	}
}

// Resuming a delete that was interrupted shows the devices and asks exactly
// like a first run; without a terminal it needs --yes like a first run.
func TestAppDeleteResumeShowsAndAsksLikeAFirstRun(t *testing.T) {
	op, fake, _ := deleteFixture(t, row(kindUnassign, vectorDevice, "packer-01"))
	fake.apps[exampleApp].DeletionStartedAt = timestamppb.Now()
	if err := runAppDelete(context.Background(), op, exampleApp, nil, false, io.Discard, false); err == nil || !strings.Contains(err.Error(), "Add --yes") || len(fake.apps) != 1 {
		t.Fatalf("no terminal, no --yes: %v", err)
	}
	var asked []string
	askConfirm = func(q string) (bool, error) { asked = append(asked, q); return strings.HasPrefix(q, "Delete "), nil }
	askText = func(q, _, _ string, _ tui.ValidateFunc) (string, error) {
		asked = append(asked, q)
		return exampleApp, nil
	}
	var out bytes.Buffer
	if err := runAppDelete(context.Background(), op, exampleApp, nil, false, &out, true); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 3 || !strings.HasPrefix(asked[0], "Delete sh.wendy.example?") || asked[2] != "Type the app name to confirm" ||
		!strings.Contains(out.String(), "is being deleted: 1 device still needs") || !strings.Contains(out.String(), ": 1 device: packer-01") || len(fake.apps) != 0 {
		t.Fatalf("asked %v\n%s", asked, out.String())
	}
}

// The outer DeleteApp binds the app; with no device to tell, one call
// deletes it after the first confirmation.
func TestAppDeleteSignsTheApp(t *testing.T) {
	op, fake, _ := deleteFixture(t)
	askConfirm = func(q string) (bool, error) { return strings.HasPrefix(q, "Delete sh.wendy.example?"), nil }
	if err := runAppDelete(context.Background(), op, exampleApp, nil, false, io.Discard, true); err != nil {
		t.Fatal(err)
	}
	calls := fake.signed(cloudpb.AppService_DeleteApp_FullMethodName)
	if len(calls) != 1 || calls[0].resource != "org/"+testOperatorTenant+"/app/"+exampleApp || len(fake.apps) != 0 {
		t.Fatalf("DeleteApp %+v", calls)
	}
}
