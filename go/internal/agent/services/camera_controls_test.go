package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Control names now come from the driver, so the spelling this API accepts is
// produced by normalising the driver's label. It must match what v4l2-ctl
// prints, because that is what anyone reading a camera's docs will type.
func TestQueryCtrlName_MatchesV4L2CtlSpelling(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"Exposure Time, Absolute", "exposure_time_absolute"},
		{"White Balance, Automatic", "white_balance_automatic"},
		{"Zoom, Absolute", "zoom_absolute"},
		{"Focus, Automatic Continuous", "focus_automatic_continuous"},
		{"Backlight Compensation", "backlight_compensation"},
		{"Gain", "gain"},
		// trailing NUL padding, leading/trailing separators, and runs of
		// punctuation must all collapse rather than leak into the name
		{"Brightness   ", "brightness"},
		{",,Hue,,", "hue"},
	}
	for _, tc := range cases {
		var q v4l2QueryCtrl
		copy(q[8:40], tc.raw)
		if got := q.name(); got != tc.want {
			t.Errorf("name(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// A name longer than the 32-byte field, or one with no terminator, must not
// read past the struct.
func TestQueryCtrlName_UnterminatedFieldStaysInBounds(t *testing.T) {
	var q v4l2QueryCtrl
	for i := 8; i < 40; i++ {
		q[i] = 'a'
	}
	if got := q.name(); len(got) != 32 {
		t.Fatalf("want the full 32 bytes, got %d (%q)", len(got), got)
	}
}

// orderControls must put the mode control before the control it gates, or the
// driver rejects the gated one (exposure_time_absolute is inactive until
// auto_exposure=Manual).
func TestOrderControls_ModeBeforeGatedControl(t *testing.T) {
	in := []controlValue{
		{name: "exposure_time_absolute", value: 20},
		{name: "backlight_compensation", value: 0},
		{name: "auto_exposure", value: 1},
	}
	out := orderControls(in)
	if out[0].name != "auto_exposure" {
		t.Fatalf("auto_exposure not first: %v", names(out))
	}
	// the two non-mode controls keep their relative order (stable sort)
	if out[1].name != "exposure_time_absolute" || out[2].name != "backlight_compensation" {
		t.Fatalf("stable order not preserved: %v", names(out))
	}
}

func names(cs []controlValue) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.name
	}
	return out
}

func TestCameraControlStore_MergeAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "camera-controls.json")
	store := newCameraControlStore(path)
	if err := store.Load(); err != nil { // absent file is fine
		t.Fatalf("Load of absent file: %v", err)
	}
	if err := store.merge("/dev/video0", []storedControl{{Name: "auto_exposure", Value: 1}}); err != nil {
		t.Fatalf("merge 1: %v", err)
	}
	// A second merge updates auto_exposure and adds exposure_time_absolute.
	if err := store.merge("/dev/video0", []storedControl{
		{Name: "auto_exposure", Value: 1},
		{Name: "exposure_time_absolute", Value: 20},
	}); err != nil {
		t.Fatalf("merge 2: %v", err)
	}

	reloaded := newCameraControlStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := reloaded.get("/dev/video0")
	if len(got) != 2 {
		t.Fatalf("want 2 stored controls, got %d: %+v", len(got), got)
	}
	byName := map[string]int32{}
	for _, c := range got {
		byName[c.Name] = c.Value
	}
	if byName["auto_exposure"] != 1 || byName["exposure_time_absolute"] != 20 {
		t.Fatalf("reloaded values wrong: %+v", byName)
	}
}

// newControlsTestService builds a VideoService with the V4L2 seams stubbed so
// the RPC logic is exercised without a real /dev/video node, and with an
// isolated on-disk store.
func newControlsTestService(t *testing.T) (*VideoService, *[]controlValue) {
	t.Helper()
	svc := NewVideoService(context.Background(), zap.NewNop())
	svc.controls = newCameraControlStore(filepath.Join(t.TempDir(), "cc.json"))
	captured := &[]controlValue{}
	svc.controlIndexFor = func(string) (map[string]uint32, error) {
		// stands in for asking the camera; the CIDs only need to be distinct
		return map[string]uint32{
			"auto_exposure":          0x009a0901,
			"exposure_time_absolute": 0x009a0902,
			"backlight_compensation": 0x0098091c,
			"brightness":             0x00980900,
			"gain":                   0x00980913,
			"zoom_absolute":          0x009a090d,
		}, nil
	}
	svc.applyLocalControls = func(_ string, ctrls []controlValue) ([]*agentpb.CameraControlResult, error) {
		*captured = ctrls
		res := make([]*agentpb.CameraControlResult, len(ctrls))
		for i, c := range ctrls {
			res[i] = &agentpb.CameraControlResult{Name: c.name, Applied: true}
		}
		return res, nil
	}
	svc.controlDefaultsFor = func(_ string, names []string) (map[string]int32, error) {
		// The driver's defaults, as QUERYCTRL would report them.
		all := map[string]int32{
			"auto_exposure": 3, "exposure_time_absolute": 156,
			"brightness": 128, "gain": 0, "zoom_absolute": 100,
		}
		out := map[string]int32{}
		for _, n := range names {
			if v, ok := all[n]; ok {
				out[n] = v
			}
		}
		return out, nil
	}
	svc.queryLocalControls = func(_ string) ([]*agentpb.CameraControl, error) {
		return []*agentpb.CameraControl{{Name: "auto_exposure", Value: 3, Minimum: 0, Maximum: 3, Mutable: true}}, nil
	}
	return svc, captured
}

func TestSetCameraControls_RejectsNetworkCamera(t *testing.T) {
	svc, _ := newControlsTestService(t)
	_, err := svc.SetCameraControls(context.Background(), &agentpb.SetCameraControlsRequest{
		DeviceId: ipcam.IDBandStart,
		Controls: []*agentpb.CameraControlSetting{{Name: "auto_exposure", Value: 1}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for a network camera, got %v", err)
	}
}

func TestSetCameraControls_UnknownControlReportedAndKnownApplied(t *testing.T) {
	svc, captured := newControlsTestService(t)
	resp, err := svc.SetCameraControls(context.Background(), &agentpb.SetCameraControlsRequest{
		DeviceId: 0,
		Controls: []*agentpb.CameraControlSetting{
			{Name: "auto_exposure", Value: 1},
			{Name: "totally_bogus", Value: 5},
		},
		Persist: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := map[string]*agentpb.CameraControlResult{}
	for _, r := range resp.GetResults() {
		got[r.GetName()] = r
	}
	// Names are resolved against the camera now, so the reason is not "nobody
	// has heard of this control" but "this camera does not have it" -- the same
	// answer a real camera gives for a control it lacks.
	if r := got["totally_bogus"]; r == nil || r.GetApplied() ||
		r.GetDetail() != "this camera has no control by that name" {
		t.Fatalf("unknown control not reported correctly: %+v", got["totally_bogus"])
	}
	if r := got["auto_exposure"]; r == nil || !r.GetApplied() {
		t.Fatalf("known control not applied: %+v", got["auto_exposure"])
	}
	// The unknown control must not reach the V4L2 seam.
	if len(*captured) != 1 || (*captured)[0].name != "auto_exposure" || (*captured)[0].cid != 0x009a0901 {
		t.Fatalf("seam received wrong controls: %v", *captured)
	}
	// persist=true stored the applied control.
	if stored := svc.controls.get("/dev/video0"); len(stored) != 1 || stored[0].Name != "auto_exposure" || stored[0].Value != 1 {
		t.Fatalf("applied control not persisted: %+v", stored)
	}
}

func TestSetCameraControls_NoControls(t *testing.T) {
	svc, _ := newControlsTestService(t)
	_, err := svc.SetCameraControls(context.Background(), &agentpb.SetCameraControlsRequest{DeviceId: 0})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for empty controls, got %v", err)
	}
}

func TestSetCameraControls_NoPersistDoesNotStore(t *testing.T) {
	svc, _ := newControlsTestService(t)
	if _, err := svc.SetCameraControls(context.Background(), &agentpb.SetCameraControlsRequest{
		DeviceId: 0,
		Controls: []*agentpb.CameraControlSetting{{Name: "gain", Value: 10}},
		Persist:  false,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stored := svc.controls.get("/dev/video0"); len(stored) != 0 {
		t.Fatalf("no-persist must not store, got %+v", stored)
	}
}

func TestGetCameraControls_RejectsNetworkCamera(t *testing.T) {
	svc, _ := newControlsTestService(t)
	_, err := svc.GetCameraControls(context.Background(), &agentpb.GetCameraControlsRequest{DeviceId: ipcam.IDBandEnd})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestGetCameraControls_ReturnsSeamOutput(t *testing.T) {
	svc, _ := newControlsTestService(t)
	resp, err := svc.GetCameraControls(context.Background(), &agentpb.GetCameraControlsRequest{DeviceId: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.GetControls()) != 1 || resp.GetControls()[0].GetName() != "auto_exposure" {
		t.Fatalf("unexpected controls: %+v", resp.GetControls())
	}
}

func TestApplyStoredCameraControls_ReAppliesResolved(t *testing.T) {
	svc, captured := newControlsTestService(t)
	if err := svc.controls.merge("/dev/video0", []storedControl{
		{Name: "auto_exposure", Value: 1},
		{Name: "exposure_time_absolute", Value: 20},
	}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	svc.applyStoredCameraControls("/dev/video0")
	if len(*captured) != 2 {
		t.Fatalf("want 2 controls re-applied, got %v", *captured)
	}
	// Resolved against the camera, not a table: every control carries a CID the
	// enumeration supplied, and two different names never share one.
	seen := map[uint32]string{}
	for _, c := range *captured {
		if c.cid == 0 {
			t.Fatalf("control %q re-applied with no cid", c.name)
		}
		if prev, dup := seen[c.cid]; dup {
			t.Fatalf("controls %q and %q share cid %#x", prev, c.name, c.cid)
		}
		seen[c.cid] = c.name
	}
}

// The store is device configuration that decides how a camera captures, so it
// is readable and writable only by the agent -- same footing as ipcam's
// credential store. It was 0o644/0o755, which let any local user read it and,
// depending on the directory, replace it.
func TestCameraControlStore_IsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "camera-controls.json")
	store := newCameraControlStore(path)
	if err := store.merge("/dev/video0", []storedControl{{Name: "gain", Value: 4}}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("store file mode = %#o, want 0600", perm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("store dir mode = %#o, want 0700", perm)
	}
}

// ── ResetCameraControls ─────────────────────────────────────────────────────
//
// Reset is its own RPC because it is a different operation from Set: it changes
// what is PERSISTED as well as the value. These pin the three things that makes
// it, none of which had a test while reset was a flag on Set.

func TestResetCameraControls_AppliesTheDriverDefault(t *testing.T) {
	svc, captured := newControlsTestService(t)
	if _, err := svc.SetCameraControls(context.Background(), &agentpb.SetCameraControlsRequest{
		DeviceId: 0,
		Controls: []*agentpb.CameraControlSetting{{Name: "brightness", Value: 200}},
		Persist:  true,
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := svc.ResetCameraControls(context.Background(), &agentpb.ResetCameraControlsRequest{
		DeviceId: 0, Names: []string{"brightness"},
	}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	// 128 is the driver's default, not the 200 that was set.
	if len(*captured) != 1 || (*captured)[0].value != 128 {
		t.Fatalf("reset wrote %+v, want brightness=128 (the driver default)", *captured)
	}
}

func TestResetCameraControls_ForgetsEvenWhenTheWriteFails(t *testing.T) {
	// The load-bearing one. A control the driver reports inactive right now
	// (exposure_time_absolute while auto_exposure is on) cannot be written. If
	// that also blocked forgetting it, the stored value would be re-asserted on
	// every reopen with NO way to clear it. Reset is two promises -- stop
	// persisting this, and put it back -- and the first must hold when the
	// second cannot.
	svc, _ := newControlsTestService(t)
	if _, err := svc.SetCameraControls(context.Background(), &agentpb.SetCameraControlsRequest{
		DeviceId: 0,
		Controls: []*agentpb.CameraControlSetting{{Name: "exposure_time_absolute", Value: 20}},
		Persist:  true,
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := svc.controls.get("/dev/video0"); len(got) != 1 {
		t.Fatalf("precondition: want 1 stored control, got %v", got)
	}

	svc.applyLocalControls = func(string, []controlValue) ([]*agentpb.CameraControlResult, error) {
		return nil, errors.New("device busy")
	}
	_, _ = svc.ResetCameraControls(context.Background(), &agentpb.ResetCameraControlsRequest{
		DeviceId: 0, Names: []string{"exposure_time_absolute"},
	})
	if got := svc.controls.get("/dev/video0"); len(got) != 0 {
		t.Errorf("a failed write kept the control stored (%v); it would be re-asserted forever", got)
	}
}

func TestResetCameraControls_NoNamesMeansEveryPersistedControl(t *testing.T) {
	// The case after a tuning session went wrong: put the camera back as it
	// shipped without having to remember what was changed.
	svc, captured := newControlsTestService(t)
	if _, err := svc.SetCameraControls(context.Background(), &agentpb.SetCameraControlsRequest{
		DeviceId: 0,
		Controls: []*agentpb.CameraControlSetting{
			{Name: "brightness", Value: 200},
			{Name: "gain", Value: 40},
		},
		Persist: true,
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := svc.ResetCameraControls(context.Background(), &agentpb.ResetCameraControlsRequest{
		DeviceId: 0, // no Names
	}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	got := map[string]int32{}
	for _, c := range *captured {
		got[c.name] = c.value
	}
	if got["brightness"] != 128 || got["gain"] != 0 {
		t.Errorf("reset-all wrote %v, want the driver defaults for both", got)
	}
	if left := svc.controls.get("/dev/video0"); len(left) != 0 {
		t.Errorf("controls still persisted after reset-all: %v", left)
	}
}

func TestResetCameraControls_RejectsNetworkCamera(t *testing.T) {
	svc, _ := newControlsTestService(t)
	_, err := svc.ResetCameraControls(context.Background(), &agentpb.ResetCameraControlsRequest{
		DeviceId: ipcam.IDBandStart, Names: []string{"gain"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for a network camera, got %v", err)
	}
}
