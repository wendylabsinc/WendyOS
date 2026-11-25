package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestPowerModeItems(t *testing.T) {
	items := powerModeItems(orinPowerModes())
	if len(items) != 4 {
		t.Fatalf("items = %d, want 4", len(items))
	}
	want := map[string]string{"MAXN": "", "MODE_15W": "", "MODE_30W": "current, boot default", "MODE_50W": ""}
	for _, it := range items {
		if it.Type != want[it.Name] {
			t.Errorf("%s status = %q, want %q", it.Name, it.Type, want[it.Name])
		}
		if it.Value != it.Name {
			t.Errorf("%s value = %v, want the mode name", it.Name, it.Value)
		}
	}
	// ID order is kept by SortKey, so 10 does not sort before 2.
	a := powerModeItems(&agentpb.GetPowerModeResponse{Available: []*agentpb.PowerMode{{Id: 10, Name: "a"}, {Id: 2, Name: "b"}}})
	if !(a[1].SortKey < a[0].SortKey) {
		t.Errorf("sort keys %q (id 10) and %q (id 2) do not order by ID", a[0].SortKey, a[1].SortKey)
	}
	// Current and boot default can be different modes.
	r := orinPowerModes()
	r.BootDefault = &agentpb.PowerMode{Id: 0, Name: "MAXN"}
	got := map[string]string{}
	for _, it := range powerModeItems(r) {
		got[it.Name] = it.Type
	}
	if got["MODE_30W"] != "current" || got["MAXN"] != "boot default" {
		t.Errorf("statuses = %v", got)
	}
}

func stubPicker(t *testing.T, name string, err error) *[]string {
	t.Helper()
	var titles []string
	orig := pickPowerModeFn
	pickPowerModeFn = func(title string, _ []tui.PickerItem) (string, error) {
		titles = append(titles, title)
		return name, err
	}
	t.Cleanup(func() { pickPowerModeFn = orig })
	return &titles
}

type waitCall struct {
	host string
	want int32
}

func stubWait(t *testing.T, applied *agentpb.PowerMode, err error) *[]waitCall {
	t.Helper()
	var calls []waitCall
	orig := waitPowerModeAppliedFn
	waitPowerModeAppliedFn = func(_ context.Context, host, _ string, want int32) (*agentpb.PowerMode, error) {
		calls = append(calls, waitCall{host, want})
		return applied, err
	}
	t.Cleanup(func() { waitPowerModeAppliedFn = orig })
	return &calls
}

func TestPowerModeScreenLiveSwitch(t *testing.T) {
	titles := stubPicker(t, "MODE_15W", nil)
	waits := stubWait(t, nil, nil)
	stubConfirm(t, true)
	f := &fakePowerModeClient{get: orinPowerModes(), setResps: []*agentpb.SetPowerModeResponse{{Mode: &agentpb.PowerMode{Id: 1, Name: "MODE_15W"}}}}
	var out bytes.Buffer
	if err := runPowerModeScreen(context.Background(), f, "thor.local", "", &out); err != nil {
		t.Fatal(err)
	}
	if len(*titles) != 1 || !strings.Contains((*titles)[0], "MODE_30W (2)") {
		t.Errorf("picker titles = %v, want the current mode shown", *titles)
	}
	if !strings.Contains(out.String(), "Power mode set to MODE_15W (1)") || len(*waits) != 0 {
		t.Errorf("out = %q, waits = %v", out.String(), *waits)
	}
}

func TestPowerModeScreenRebootWaitsForTheNewMode(t *testing.T) {
	stubPicker(t, "MAXN", nil)
	asked := stubConfirm(t, true)
	maxn := &agentpb.PowerMode{Id: 0, Name: "MAXN"}
	waits := stubWait(t, maxn, nil)
	f := &fakePowerModeClient{get: orinPowerModes(), setResps: []*agentpb.SetPowerModeResponse{
		{Mode: maxn, RebootRequired: true},
		{Mode: maxn, RebootRequired: true, RebootScheduled: true},
	}}
	var out bytes.Buffer
	if err := runPowerModeScreen(context.Background(), f, "thor.local", "", &out); err != nil {
		t.Fatal(err)
	}
	if len(*asked) != 1 || len(f.setReqs) != 2 || !f.setReqs[1].GetReboot() {
		t.Errorf("asked=%v requests=%v", *asked, f.setReqs)
	}
	if len(*waits) != 1 || (*waits)[0] != (waitCall{"thor.local", 0}) {
		t.Errorf("waits = %v, want one wait on thor.local for id 0", *waits)
	}
	if !strings.Contains(out.String(), "rebooting now") || !strings.Contains(out.String(), "Back online. Power mode is now MAXN (0)") {
		t.Errorf("out = %q", out.String())
	}
}

func TestPowerModeScreenDeclinedRebootChangesNothing(t *testing.T) {
	stubPicker(t, "MAXN", nil)
	stubConfirm(t, false)
	waits := stubWait(t, nil, nil)
	f := &fakePowerModeClient{get: orinPowerModes(), setResps: []*agentpb.SetPowerModeResponse{{Mode: maxnMode, RebootRequired: true}}}
	var out bytes.Buffer
	if err := runPowerModeScreen(context.Background(), f, "h", "", &out); err != nil {
		t.Fatal(err)
	}
	if len(f.setReqs) != 1 || f.setReqs[0].GetReboot() || len(*waits) != 0 || !strings.Contains(out.String(), "unchanged") {
		t.Errorf("requests=%v waits=%v out=%q", f.setReqs, *waits, out.String())
	}
}

func TestPowerModeScreenPickingTheCurrentModeIsANoOp(t *testing.T) {
	stubPicker(t, "MODE_30W", nil)
	f := &fakePowerModeClient{get: orinPowerModes()}
	var out bytes.Buffer
	if err := runPowerModeScreen(context.Background(), f, "h", "", &out); err != nil {
		t.Fatal(err)
	}
	if len(f.setReqs) != 0 || !strings.Contains(out.String(), "Already in MODE_30W (2)") {
		t.Errorf("requests=%v out=%q", f.setReqs, out.String())
	}
}

func TestPowerModeScreenCancelAndWaitFailure(t *testing.T) {
	stubPicker(t, "", ErrUserCancelled)
	f := &fakePowerModeClient{get: orinPowerModes()}
	if err := runPowerModeScreen(context.Background(), f, "h", "", &bytes.Buffer{}); !errors.Is(err, ErrUserCancelled) || len(f.setReqs) != 0 {
		t.Errorf("cancel: err=%v requests=%v", err, f.setReqs)
	}

	stubPicker(t, "MAXN", nil)
	stubConfirm(t, true)
	stubWait(t, nil, errors.New("did not come back"))
	f = &fakePowerModeClient{get: orinPowerModes(), setResps: []*agentpb.SetPowerModeResponse{
		needsReboot(), {Mode: maxnMode, RebootRequired: true, RebootScheduled: true},
	}}
	if err := runPowerModeScreen(context.Background(), f, "h", "", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "did not come back") {
		t.Errorf("wait failure: err = %v", err)
	}
}

func TestPollPowerModeAppliedIgnoresStaleReadingsAndErrors(t *testing.T) {
	seq := []struct {
		mode *agentpb.PowerMode
		err  error
	}{
		{&agentpb.PowerMode{Id: 2, Name: "old"}, nil},                 // still up, before the reboot
		{nil, &noAuthenticatedEndpointError{msg: "nothing answered"}}, // rebooting
		{&agentpb.PowerMode{Id: 0, Name: "MAXN"}, nil},
	}
	i := 0
	probe := func(context.Context) (*agentpb.PowerMode, error) {
		s := seq[i]
		i++
		return s.mode, s.err
	}
	got, err := pollPowerModeApplied(context.Background(), probe, 0, time.Millisecond)
	if err != nil || got.GetName() != "MAXN" || i != 3 {
		t.Fatalf("got=%v err=%v probes=%d", got, err, i)
	}
}

func TestPollPowerModeAppliedTimesOutWithTheLastReading(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	probe := func(context.Context) (*agentpb.PowerMode, error) {
		return &agentpb.PowerMode{Id: 2, Name: "MODE_30W"}, nil
	}
	_, err := pollPowerModeApplied(ctx, probe, 0, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "last seen MODE_30W (2)") {
		t.Errorf("err = %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	_, err = pollPowerModeApplied(ctx2, func(context.Context) (*agentpb.PowerMode, error) { return nil, errors.New("down") }, 0, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "never answered") {
		t.Errorf("err = %v", err)
	}
}

func TestPowerModeGet(t *testing.T) {
	f := &fakePowerModeClient{get: orinPowerModes()}
	var out bytes.Buffer
	if err := runPowerModeGet(context.Background(), f, &out, false); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "MODE_30W (2)" {
		t.Errorf("human = %q", out.String())
	}
	out.Reset()
	if err := runPowerModeGet(context.Background(), f, &out, true); err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc["current"]["name"] != "MODE_30W" || doc["current"]["id"] != float64(2) {
		t.Errorf("json = %q (%v)", out.String(), err)
	}
	if _, has := doc["available"]; has {
		t.Error("get must print only the current mode")
	}
}

func TestPowerModeCommandHasExplicitSubcommands(t *testing.T) {
	cmd := newDevicePowerModeCmd()
	for _, name := range []string{"list", "get", "set"} {
		sub, _, err := cmd.Find([]string{name})
		if err != nil || sub.Name() != name {
			t.Errorf("subcommand %q not found: %v", name, err)
		}
	}
}
