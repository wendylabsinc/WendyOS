package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type fakePowerModeClient struct {
	agentpb.WendyAgentServiceClient
	get *agentpb.GetPowerModeResponse
	// setResps/setErrs script successive SetPowerMode calls; the last entry
	// repeats. setReqs records every request.
	setResps []*agentpb.SetPowerModeResponse
	setErrs  []error
	err      error
	setReqs  []*agentpb.SetPowerModeRequest
}

func (f *fakePowerModeClient) GetPowerMode(context.Context, *agentpb.GetPowerModeRequest, ...grpc.CallOption) (*agentpb.GetPowerModeResponse, error) {
	return f.get, f.err
}

func (f *fakePowerModeClient) SetPowerMode(_ context.Context, req *agentpb.SetPowerModeRequest, _ ...grpc.CallOption) (*agentpb.SetPowerModeResponse, error) {
	i := len(f.setReqs)
	f.setReqs = append(f.setReqs, req)
	if f.err != nil {
		return nil, f.err
	}
	if i >= len(f.setResps) {
		i = len(f.setResps) - 1
	}
	var err error
	if i < len(f.setErrs) {
		err = f.setErrs[i]
	}
	return f.setResps[i], err
}

func orinPowerModes() *agentpb.GetPowerModeResponse {
	return &agentpb.GetPowerModeResponse{
		Current: &agentpb.PowerMode{Id: 2, Name: "MODE_30W"},
		Available: []*agentpb.PowerMode{
			{Id: 0, Name: "MAXN"}, {Id: 1, Name: "MODE_15W"}, {Id: 2, Name: "MODE_30W"}, {Id: 3, Name: "MODE_50W"},
		},
		BootDefault: &agentpb.PowerMode{Id: 2, Name: "MODE_30W"},
	}
}

func TestPowerModeShowHuman(t *testing.T) {
	var out bytes.Buffer
	if err := runPowerModeShow(context.Background(), &fakePowerModeClient{get: orinPowerModes()}, &out, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"MODE_30W (2)", "MAXN", "MODE_50W", "current, boot default"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestPowerModeShowJSON(t *testing.T) {
	var out bytes.Buffer
	if err := runPowerModeShow(context.Background(), &fakePowerModeClient{get: orinPowerModes()}, &out, true); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Current     map[string]any   `json:"current"`
		Available   []map[string]any `json:"available"`
		BootDefault map[string]any   `json:"bootDefault"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if doc.Current["name"] != "MODE_30W" || doc.Current["id"] != float64(2) || len(doc.Available) != 4 || doc.BootDefault["name"] != "MODE_30W" {
		t.Errorf("doc = %+v", doc)
	}
}

var maxnMode = &agentpb.PowerMode{Id: 0, Name: "MAXN"}

func stubConfirm(t *testing.T, answer bool) *[]string {
	t.Helper()
	var asked []string
	orig := confirmDefaultNoFn
	confirmDefaultNoFn = func(q string) bool { asked = append(asked, q); return answer }
	t.Cleanup(func() { confirmDefaultNoFn = orig })
	return &asked
}

func needsReboot() *agentpb.SetPowerModeResponse {
	return &agentpb.SetPowerModeResponse{Mode: maxnMode, RebootRequired: true}
}

func TestPowerModeSetLiveSwitchNeverPrompts(t *testing.T) {
	asked := stubConfirm(t, true)
	f := &fakePowerModeClient{setResps: []*agentpb.SetPowerModeResponse{{Mode: maxnMode}}}
	var out bytes.Buffer
	if err := runPowerModeSet(context.Background(), f, "MAXN", false, true, &out, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Power mode set to MAXN (0)") || len(*asked) != 0 || len(f.setReqs) != 1 || f.setReqs[0].GetReboot() {
		t.Errorf("out=%q asked=%v reqs=%v", out.String(), *asked, f.setReqs)
	}
}

func TestPowerModeSetConfirmedSetsAndRebootsInOneStep(t *testing.T) {
	asked := stubConfirm(t, true)
	f := &fakePowerModeClient{setResps: []*agentpb.SetPowerModeResponse{
		needsReboot(),
		{Mode: maxnMode, RebootRequired: true, RebootScheduled: true},
	}}
	var out bytes.Buffer
	if err := runPowerModeSet(context.Background(), f, "MAXN", false, true, &out, false); err != nil {
		t.Fatal(err)
	}
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "MAXN (0)") || !strings.Contains((*asked)[0], "reboot") {
		t.Errorf("prompt = %v", *asked)
	}
	if len(f.setReqs) != 2 || f.setReqs[0].GetReboot() || !f.setReqs[1].GetReboot() {
		t.Errorf("requests = %v, want a probe then a reboot=true set", f.setReqs)
	}
	if !strings.Contains(out.String(), "rebooting now") {
		t.Errorf("output = %q", out.String())
	}
}

func TestPowerModeSetDeclinedChangesNothing(t *testing.T) {
	stubConfirm(t, false)
	f := &fakePowerModeClient{setResps: []*agentpb.SetPowerModeResponse{needsReboot()}}
	var out bytes.Buffer
	if err := runPowerModeSet(context.Background(), f, "MAXN", false, true, &out, false); err != nil {
		t.Fatal(err)
	}
	if len(f.setReqs) != 1 || f.setReqs[0].GetReboot() {
		t.Errorf("requests = %v, want only the non-rebooting probe", f.setReqs)
	}
	if !strings.Contains(out.String(), "unchanged") {
		t.Errorf("output = %q", out.String())
	}
}

func TestPowerModeSetWithoutTerminalNeedsYes(t *testing.T) {
	asked := stubConfirm(t, true)
	for _, asJSON := range []bool{false, true} {
		f := &fakePowerModeClient{setResps: []*agentpb.SetPowerModeResponse{needsReboot()}}
		var out bytes.Buffer
		err := runPowerModeSet(context.Background(), f, "MAXN", false, false, &out, asJSON)
		if err == nil || !strings.Contains(err.Error(), "--yes") || !strings.Contains(err.Error(), "was not set") {
			t.Errorf("json=%v err = %v, want a --yes hint", asJSON, err)
		}
		if len(f.setReqs) != 1 || f.setReqs[0].GetReboot() || len(*asked) != 0 {
			t.Errorf("json=%v requests=%v asked=%v", asJSON, f.setReqs, *asked)
		}
	}
}

func TestPowerModeSetYesSkipsThePrompt(t *testing.T) {
	asked := stubConfirm(t, false)
	f := &fakePowerModeClient{setResps: []*agentpb.SetPowerModeResponse{
		needsReboot(),
		{Mode: maxnMode, RebootRequired: true, RebootScheduled: true},
	}}
	var out bytes.Buffer
	if err := runPowerModeSet(context.Background(), f, "MAXN", true, false, &out, true); err != nil {
		t.Fatal(err)
	}
	if len(*asked) != 0 || len(f.setReqs) != 2 || !f.setReqs[1].GetReboot() {
		t.Errorf("asked=%v requests=%v", *asked, f.setReqs)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if doc["rebootRequired"] != true || doc["rebootScheduled"] != true || doc["mode"].(map[string]any)["name"] != "MAXN" {
		t.Errorf("doc = %v", doc)
	}
}

func TestPowerModeSetDeviceDroppingTheConnectionMeansRebooting(t *testing.T) {
	f := &fakePowerModeClient{
		setResps: []*agentpb.SetPowerModeResponse{needsReboot(), nil},
		setErrs:  []error{nil, status.Error(codes.Unavailable, "error reading from server: EOF")},
	}
	var out bytes.Buffer
	if err := runPowerModeSet(context.Background(), f, "MAXN", true, false, &out, false); err != nil {
		t.Fatalf("a dropped connection after the confirmed call is the reboot, got %v", err)
	}
	if !strings.Contains(out.String(), "rebooting now") {
		t.Errorf("output = %q", out.String())
	}
}

func TestPowerModeSetLiveJSON(t *testing.T) {
	f := &fakePowerModeClient{setResps: []*agentpb.SetPowerModeResponse{{Mode: maxnMode}}}
	var out bytes.Buffer
	if err := runPowerModeSet(context.Background(), f, "0", false, false, &out, true); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["rebootRequired"] != false || doc["rebootScheduled"] != false || doc["mode"].(map[string]any)["name"] != "MAXN" {
		t.Errorf("doc = %v", doc)
	}
}

func TestPowerModeErrors(t *testing.T) {
	cases := []struct {
		code codes.Code
		msg  string
		want string
	}{
		{codes.Unimplemented, "method not implemented", "update the agent"},
		{codes.Unavailable, "power modes are only supported on NVIDIA Jetson devices", "only supported on NVIDIA Jetson"},
		{codes.InvalidArgument, `unknown power mode "x"; available: MAXN (0)`, `unknown power mode "x"`},
	}
	for _, tc := range cases {
		f := &fakePowerModeClient{err: status.Error(tc.code, tc.msg)}
		var out bytes.Buffer
		for name, err := range map[string]error{
			"show": runPowerModeShow(context.Background(), f, &out, false),
			"set":  runPowerModeSet(context.Background(), f, "x", false, false, &out, false),
		} {
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s %v: err = %v, want containing %q", name, tc.code, err, tc.want)
			}
		}
	}
}

func TestPowerModeCommandShape(t *testing.T) {
	cmd := newDevicePowerModeCmd()
	set, _, err := cmd.Find([]string{"set"})
	if err != nil || set.Name() != "set" {
		t.Fatalf("set subcommand not found: %v", err)
	}
	yes := set.Flags().Lookup("yes")
	if yes == nil || yes.Shorthand != "y" {
		t.Fatal("set has no --yes/-y flag")
	}
	if yes.DefValue != "false" {
		t.Error("--yes must default to false")
	}
	if set.Flags().Lookup("reboot") != nil {
		t.Error("--reboot was replaced by --yes")
	}
	if err := set.Args(set, nil); err == nil {
		t.Error("set without a mode should be rejected")
	}
}
