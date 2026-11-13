package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/arduinotoolchain"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type fakeFlashStream struct {
	grpc.ClientStream
	sent      []*agentpbv2.FlashSketchRequest
	responses []*agentpbv2.FlashSketchResponse
	closed    bool
}

func (f *fakeFlashStream) Send(r *agentpbv2.FlashSketchRequest) error {
	f.sent = append(f.sent, r)
	return nil
}

func (f *fakeFlashStream) CloseSend() error {
	f.closed = true
	return nil
}

func (f *fakeFlashStream) Recv() (*agentpbv2.FlashSketchResponse, error) {
	if len(f.responses) == 0 {
		return nil, io.EOF
	}
	r := f.responses[0]
	f.responses = f.responses[1:]
	return r, nil
}

type fakeMcuClient struct {
	stream *fakeFlashStream
	calls  int
}

func (f *fakeMcuClient) FlashSketch(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[agentpbv2.FlashSketchRequest, agentpbv2.FlashSketchResponse], error) {
	f.calls++
	return f.stream, nil
}

func completed(c *agentpbv2.FlashSketchResponse_Completed) []*agentpbv2.FlashSketchResponse {
	return []*agentpbv2.FlashSketchResponse{
		{ResponseType: &agentpbv2.FlashSketchResponse_Progress_{Progress: &agentpbv2.FlashSketchResponse_Progress{Phase: "flashing", LogLine: "Info : flashing"}}},
		{ResponseType: &agentpbv2.FlashSketchResponse_Completed_{Completed: c}},
	}
}

type mcuHarness struct {
	conn     *grpcclient.AgentConnection
	version  *fakeAgentVersionClient
	mcu      *fakeMcuClient
	compiles int
	cfg      *appconfig.AppConfig
	cwd      string
}

func newMCUHarness(t *testing.T, deviceType string, features []string, responses []*agentpbv2.FlashSketchResponse) *mcuHarness {
	t.Helper()
	dir := t.TempDir()
	loader := filepath.Join(dir, "loader.elf")
	sketch := filepath.Join(dir, "sketch.bin")
	if err := os.WriteFile(loader, []byte("loader"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sketch, []byte("sketch"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, dir, "1.1.0")
	h := &mcuHarness{
		version: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{DeviceType: &deviceType, Featureset: features}},
		mcu:     &fakeMcuClient{stream: &fakeFlashStream{responses: responses}},
		cfg:     &appconfig.AppConfig{AppID: "sh.wendy.examples.modulino", MCU: &appconfig.MCUConfig{Sketch: "sketch"}},
		cwd:     dir,
	}
	h.conn = &grpcclient.AgentConnection{AgentService: h.version, McuService: h.mcu}

	origCompile, origHost := compileSketchFn, checkArduinoHostFn
	t.Cleanup(func() { compileSketchFn, checkArduinoHostFn = origCompile, origHost })
	// The real check probes Rosetta on Apple Silicon; results must not depend on the host.
	checkArduinoHostFn = func(context.Context) error { return nil }
	compileSketchFn = func(_ context.Context, sketchDir, profile string, _ io.Writer) (*arduinotoolchain.Artifacts, error) {
		h.compiles++
		if sketchDir != filepath.Join(dir, "sketch") {
			t.Errorf("compiled %q, want the project's sketch dir", sketchDir)
		}
		return &arduinotoolchain.Artifacts{LoaderPath: loader, SketchPath: sketch, CoreVersion: "1.1.0", Layout: "zephyr-1"}, nil
	}
	return h
}

func writeProfile(t *testing.T, dir, core string) {
	t.Helper()
	sk := filepath.Join(dir, "sketch")
	if err := os.MkdirAll(sk, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "profiles:\n  default:\n    fqbn: arduino:zephyr:unoq\n    platforms:\n      - platform: arduino:zephyr (" + core + ")\ndefault_profile: default\n"
	if err := os.WriteFile(filepath.Join(sk, "sketch.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func countRegistrations(t *testing.T) *int {
	t.Helper()
	n := 0
	orig := registerCloudAppsFn
	t.Cleanup(func() { registerCloudAppsFn = orig })
	registerCloudAppsFn = func(context.Context, *grpcclient.AgentConnection, []string, bool) error {
		n++
		return nil
	}
	return &n
}

// deploy runs the MCU steps of runWithAgent: preflight, then compile and flash.
func (h *mcuHarness) deploy(opts runOptions) error {
	plan, err := preflightMCU(context.Background(), h.conn, h.cwd, h.cfg, opts)
	if err != nil {
		return err
	}
	return deployMCU(context.Background(), h.conn, plan, h.cfg, opts)
}

func TestDeployMCU_NoMCUFieldMakesNoRPC(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"}, nil)
	h.cfg.MCU = nil
	var err error
	out := captureStderr(t, func() { err = h.deploy(runOptions{}) })
	if err != nil {
		t.Fatalf("deployMCU() = %v, want nil", err)
	}
	if h.version.calls != 0 || h.mcu.calls != 0 || h.compiles != 0 || out != "" {
		t.Fatalf("version calls %d, flash calls %d, compiles %d, output %q; want nothing at all",
			h.version.calls, h.mcu.calls, h.compiles, out)
	}
}

func TestDeployMCU_OtherBoardRejected(t *testing.T) {
	h := newMCUHarness(t, "raspberrypi5", []string{"gpu"}, nil)
	err := h.deploy(runOptions{})
	if err == nil || !strings.Contains(err.Error(), "only Arduino UNO Q") {
		t.Fatalf("deployMCU() = %v, want an only-Arduino-UNO-Q error", err)
	}
	if h.compiles != 0 || h.mcu.calls != 0 {
		t.Fatalf("compiles %d, flash calls %d; want none", h.compiles, h.mcu.calls)
	}
}

func TestDeployMCU_OldUnoQRejected(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"gpu"}, nil)
	if err := h.deploy(runOptions{}); err == nil || !strings.Contains(err.Error(), "wendy device update") {
		t.Fatalf("deployMCU() = %v, want an update error", err)
	}
	if h.compiles != 0 {
		t.Fatalf("compiles = %d, want 0", h.compiles)
	}
}

func TestDeployMCU_CompileErrorMakesNoRPC(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"}, nil)
	compileSketchFn = func(context.Context, string, string, io.Writer) (*arduinotoolchain.Artifacts, error) {
		return nil, errors.New("sketch.ino:3: error: expected ';'")
	}
	if err := h.deploy(runOptions{}); err == nil || !strings.Contains(err.Error(), "expected ';'") {
		t.Fatalf("deployMCU() = %v, want the compile error", err)
	}
	if h.mcu.calls != 0 {
		t.Fatalf("FlashSketch calls = %d, want 0", h.mcu.calls)
	}
}

func TestDeployMCU_UnsupportedLayout(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"}, nil)
	orig := compileSketchFn
	compileSketchFn = func(ctx context.Context, dir, profile string, w io.Writer) (*arduinotoolchain.Artifacts, error) {
		a, err := orig(ctx, dir, profile, w)
		a.Layout, a.CoreVersion = "zephyr-2", "2.0.0"
		return a, err
	}
	err := h.deploy(runOptions{})
	if err == nil || !strings.Contains(err.Error(), "update WendyOS") || !strings.Contains(err.Error(), "2.0.0") {
		t.Fatalf("deployMCU() = %v, want an update-WendyOS error naming the core", err)
	}
	if h.mcu.calls != 0 {
		t.Fatalf("FlashSketch calls = %d, want 0", h.mcu.calls)
	}
}

func TestFlashMCU_SendsSpecThenChunks(t *testing.T) {
	dir := t.TempDir()
	loader := bytes.Repeat([]byte{1}, 3<<20)
	sketch := bytes.Repeat([]byte{2}, 10<<10)
	lp, sp := filepath.Join(dir, "l.elf"), filepath.Join(dir, "s.bin")
	_ = os.WriteFile(lp, loader, 0o644)
	_ = os.WriteFile(sp, sketch, 0o644)
	stream := &fakeFlashStream{responses: completed(&agentpbv2.FlashSketchResponse_Completed{SketchWritten: true, RouterActive: true})}
	spec, err := mcuSpec("sh.app", "zephyr-1", lp, sp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flashMCU(context.Background(), &fakeMcuClient{stream: stream}, spec, lp, sp, io.Discard); err != nil {
		t.Fatalf("flashMCU: %v", err)
	}
	if len(stream.sent) != 5 || stream.sent[0].GetSpec() == nil {
		t.Fatalf("sent %d messages, want the spec then 4 chunks", len(stream.sent))
	}
	var got []byte
	for _, r := range stream.sent[1:] {
		got = append(got, r.GetChunk().GetData()...)
	}
	if !bytes.Equal(got, append(append([]byte{}, loader...), sketch...)) {
		t.Fatal("chunks are not the loader followed by the sketch")
	}
	s := stream.sent[0].GetSpec()
	l256, s256 := sha256.Sum256(loader), sha256.Sum256(sketch)
	if s.GetLoader().GetSha256() != hex.EncodeToString(l256[:]) || s.GetSketch().GetSha256() != hex.EncodeToString(s256[:]) ||
		s.GetLoader().GetSize() != uint64(len(loader)) || s.GetSketch().GetSize() != uint64(len(sketch)) || s.GetAppId() != "sh.app" {
		t.Fatalf("spec = %+v, want sizes and sha256 of both files", s)
	}
	if !stream.closed {
		t.Fatal("send side never closed")
	}
}

func TestDeployMCU_WarnsWhenRouterInactive(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"},
		completed(&agentpbv2.FlashSketchResponse_Completed{SketchWritten: true, RouterActive: false}))
	var err error
	out := captureStderr(t, func() { err = h.deploy(runOptions{}) })
	if err != nil {
		t.Fatalf("deployMCU: %v", err)
	}
	if !strings.Contains(out, "Warning: Arduino router is not running; the sketch starts once it is") {
		t.Fatalf("output %q lacks the router warning", out)
	}
}

func TestDeployMCU_SummaryNamesReplacedApp(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"},
		completed(&agentpbv2.FlashSketchResponse_Completed{SketchWritten: true, RouterActive: true, PreviousAppId: "old.app"}))
	var err error
	out := captureStderr(t, func() { err = h.deploy(runOptions{}) })
	if err != nil {
		t.Fatalf("deployMCU: %v", err)
	}
	if h.version.calls != 1 {
		t.Errorf("version queries = %d, want 1", h.version.calls)
	}
	for _, want := range []string{"sketch written", "loader unchanged", "replaced the sketch of old.app and stopped it"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "Warning") {
		t.Errorf("output %q warns with the router running", out)
	}
}

func TestDeployMCU_WatchSkipsAnUnchangedSketch(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"},
		completed(&agentpbv2.FlashSketchResponse_Completed{SketchWritten: true, RouterActive: true}))
	opts := runOptions{watchState: &watchDeployState{hashes: map[string]string{}}}
	if err := h.deploy(opts); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	owned := func(app string) {
		h.version.resp.Featureset = []string{"mcu-flash:zephyr-1", mcuOwnerPrefix + app}
	}
	owned(h.cfg.AppID)
	var err error
	out := captureStderr(t, func() { err = h.deploy(opts) })
	if err != nil || h.mcu.calls != 1 || h.compiles != 1 || !strings.Contains(out, "unchanged since the last flash") {
		t.Fatalf("second deploy = %v, flash calls %d, compiles %d, output %q; want one compile and flash, then a skip", err, h.mcu.calls, h.compiles, out)
	}
	// Another app took the chip since: the unchanged sketch is flashed again.
	owned("other.app")
	h.mcu.stream.responses = completed(&agentpbv2.FlashSketchResponse_Completed{SketchWritten: true, RouterActive: true})
	if err := h.deploy(opts); err != nil || h.mcu.calls != 2 {
		t.Fatalf("deploy after another app took the chip = %v, flash calls %d; want a flash", err, h.mcu.calls)
	}
	// An interrupted flash by someone else may have damaged it: flashed again.
	h.version.resp.Featureset = append(h.version.resp.Featureset[:1], mcuOwnerPrefix+h.cfg.AppID, mcuContentUnknownFeature)
	h.mcu.stream.responses = completed(&agentpbv2.FlashSketchResponse_Completed{SketchWritten: true, RouterActive: true})
	if err := h.deploy(opts); err != nil || h.mcu.calls != 3 {
		t.Fatalf("deploy after an interrupted flash = %v, flash calls %d; want a flash", err, h.mcu.calls)
	}
	owned(h.cfg.AppID)
	// A changed sketch file, or another board at the same address, is flashed.
	if err := os.WriteFile(filepath.Join(h.cwd, "sketch", "sketch.ino"), []byte("void setup(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mcu.stream.responses = completed(&agentpbv2.FlashSketchResponse_Completed{SketchWritten: true, RouterActive: true})
	if err := h.deploy(opts); err != nil || h.mcu.calls != 4 {
		t.Fatalf("deploy after a sketch edit = %v, flash calls %d; want a flash", err, h.mcu.calls)
	}
	other := "other-board-key"
	h.version.resp.PublicKey = &other
	h.conn.CacheAgentVersion(h.version.resp)
	h.mcu.stream.responses = completed(&agentpbv2.FlashSketchResponse_Completed{SketchWritten: true, RouterActive: true})
	if err := h.deploy(opts); err != nil || h.mcu.calls != 5 {
		t.Fatalf("deploy to another board = %v, flash calls %d; want a flash", err, h.mcu.calls)
	}
	h.mcu.stream.responses = completed(&agentpbv2.FlashSketchResponse_Completed{RouterActive: true})
	if err := h.deploy(runOptions{}); err != nil || h.mcu.calls != 6 {
		t.Fatalf("deploy outside watch = %v, flash calls %d; want a flash", err, h.mcu.calls)
	}
}

func TestDeployMCU_WatchFlashesAgainAfterAFailedFlash(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"},
		completed(&agentpbv2.FlashSketchResponse_Completed{SketchWritten: true, RouterActive: true}))
	opts := runOptions{watchState: &watchDeployState{hashes: map[string]string{}}}
	if err := h.deploy(opts); err != nil {
		t.Fatalf("flash A: %v", err)
	}
	ino := filepath.Join(h.cwd, "sketch", "sketch.ino")
	if err := os.WriteFile(ino, []byte("// B"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mcu.stream.responses = []*agentpbv2.FlashSketchResponse{{ResponseType: &agentpbv2.FlashSketchResponse_Failed_{
		Failed: &agentpbv2.FlashSketchResponse_Failed{ErrorMessage: "OpenOCD failed (exit 1)"}}}}
	if err := h.deploy(opts); err == nil {
		t.Fatal("flash B succeeded, want the failure")
	}
	// Back to A: the failed flash may have left B half-written, so A must be flashed again.
	if err := os.Remove(ino); err != nil {
		t.Fatal(err)
	}
	h.mcu.stream.responses = completed(&agentpbv2.FlashSketchResponse_Completed{SketchWritten: true, RouterActive: true})
	if err := h.deploy(opts); err != nil || h.mcu.calls != 3 {
		t.Fatalf("flash A again = %v, flash calls %d; want 3 flashes", err, h.mcu.calls)
	}
}

func TestPreflightMCU_ChecksTheSelectedServicesEntitlements(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"}, nil)
	h.version.resp.EntitlementTypes = []string{"network"}
	h.cfg.Services = map[string]*appconfig.ServiceConfig{
		"api": {Entitlements: []appconfig.Entitlement{{Type: "episode-write"}}},
		"web": {},
	}
	if err := h.deploy(runOptions{}); err == nil || !strings.Contains(err.Error(), "episode-write") {
		t.Fatalf("deploy = %v, want the service's unsupported entitlement", err)
	}
	if h.compiles != 0 {
		t.Fatalf("compiles = %d, want 0", h.compiles)
	}
	if _, err := preflightMCU(context.Background(), h.conn, h.cwd, h.cfg, runOptions{service: "web"}); err != nil {
		t.Fatalf("preflight for --service web = %v, want nil: api is not deployed", err)
	}
}

func TestPreflightMCU_RefusesFleetRuns(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"}, nil)
	err := h.deploy(runOptions{fleetDevices: []string{"unoq-2"}})
	if err == nil || !strings.Contains(err.Error(), "several devices") || ErrorClass(err) != "project_target_mismatch" {
		t.Fatalf("deploy = %v (class %q), want the fleet refusal", err, ErrorClass(err))
	}
	if h.version.calls != 0 || h.compiles != 0 || h.mcu.calls != 0 {
		t.Fatalf("version calls %d, compiles %d, flash calls %d; want none", h.version.calls, h.compiles, h.mcu.calls)
	}
}

func TestMCURefusalsCarryAnErrorClass(t *testing.T) {
	pi := newMCUHarness(t, "raspberrypi5", []string{"gpu"}, nil)
	if err := pi.deploy(runOptions{}); ErrorClass(err) != "project_target_mismatch" {
		t.Errorf("other board: class %q, want project_target_mismatch", ErrorClass(err))
	}
	noYAML := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"}, nil)
	if err := os.Remove(filepath.Join(noYAML.cwd, "sketch", "sketch.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := noYAML.deploy(runOptions{}); ErrorClass(err) != "config_invalid" {
		t.Errorf("missing sketch.yaml: class %q, want config_invalid", ErrorClass(err))
	}
	broken := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"}, nil)
	compileSketchFn = func(context.Context, string, string, io.Writer) (*arduinotoolchain.Artifacts, error) {
		return nil, errors.New("sketch.ino:3: error")
	}
	if err := broken.deploy(runOptions{}); ErrorClass(err) != "build_failed" {
		t.Errorf("compile error: class %q, want build_failed", ErrorClass(err))
	}
}

func TestDeployMCU_FailedStopsDeploy(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"},
		[]*agentpbv2.FlashSketchResponse{{ResponseType: &agentpbv2.FlashSketchResponse_Failed_{
			Failed: &agentpbv2.FlashSketchResponse_Failed{ErrorMessage: "OpenOCD failed (exit 1)"}}}})
	recorded := 0
	orig := runWithAgentRecordedFn
	t.Cleanup(func() { runWithAgentRecordedFn = orig })
	runWithAgentRecordedFn = func(context.Context, *grpcclient.AgentConnection, string, *appconfig.AppConfig, runOptions, *deployMetrics) error {
		recorded++
		return nil
	}
	err := runWithAgent(context.Background(), h.conn, h.cwd, h.cfg, runOptions{skipCloudRegistration: true})
	if err == nil || !strings.Contains(err.Error(), "OpenOCD failed") {
		t.Fatalf("runWithAgent() = %v, want the flash failure", err)
	}
	if recorded != 0 {
		t.Fatalf("container deploy ran %d times after a failed flash, want 0", recorded)
	}
}

func TestRunWithAgent_RefusedMCUProjectRegistersNothing(t *testing.T) {
	h := newMCUHarness(t, "raspberrypi5", []string{"gpu"}, nil)
	registered := 0
	orig := registerCloudAppsFn
	t.Cleanup(func() { registerCloudAppsFn = orig })
	registerCloudAppsFn = func(context.Context, *grpcclient.AgentConnection, []string, bool) error {
		registered++
		return nil
	}
	err := runWithAgent(context.Background(), h.conn, h.cwd, h.cfg, runOptions{})
	if err == nil || !strings.Contains(err.Error(), "only Arduino UNO Q") {
		t.Fatalf("runWithAgent() = %v, want the board refusal", err)
	}
	if registered != 0 {
		t.Fatalf("cloud app registered %d times for a refused project, want 0", registered)
	}
}

func TestRunWithAgent_MissingSketchYAMLRefusedBeforeRegistration(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"}, nil)
	if err := os.Remove(filepath.Join(h.cwd, "sketch", "sketch.yaml")); err != nil {
		t.Fatal(err)
	}
	registered := countRegistrations(t)
	err := runWithAgent(context.Background(), h.conn, h.cwd, h.cfg, runOptions{})
	if err == nil || !strings.Contains(err.Error(), "sketch.yaml") {
		t.Fatalf("runWithAgent() = %v, want a sketch.yaml error", err)
	}
	if *registered != 0 || h.compiles != 0 {
		t.Fatalf("registrations %d, compiles %d; want 0 and 0", *registered, h.compiles)
	}
}

func TestRunWithAgent_OldCorePinRefusedBeforeCompile(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"}, nil)
	writeProfile(t, h.cwd, "0.53.1")
	registered := countRegistrations(t)
	err := runWithAgent(context.Background(), h.conn, h.cwd, h.cfg, runOptions{})
	if err == nil || !strings.Contains(err.Error(), "unsupported Arduino core version 0.53.1") {
		t.Fatalf("runWithAgent() = %v, want the unsupported-core error", err)
	}
	if *registered != 0 || h.compiles != 0 {
		t.Fatalf("registrations %d, compiles %d; want 0 and 0", *registered, h.compiles)
	}
}

func TestRunWithAgent_UnknownEntitlementRefusedBeforeFlash(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"}, nil)
	h.version.resp.EntitlementTypes = []string{"network", "http"}
	h.cfg.Entitlements = []appconfig.Entitlement{{Type: appconfig.EntitlementArduinoBridge}}
	countRegistrations(t)
	err := runWithAgent(context.Background(), h.conn, h.cwd, h.cfg, runOptions{})
	if err == nil || !strings.Contains(err.Error(), "arduino-bridge") {
		t.Fatalf("runWithAgent() = %v, want an arduino-bridge entitlement error", err)
	}
	if h.mcu.calls != 0 || h.compiles != 0 {
		t.Fatalf("flash calls %d, compiles %d; want 0 and 0", h.mcu.calls, h.compiles)
	}
}

func TestRejectMCUOnProviderTarget(t *testing.T) {
	if err := rejectMCUForProvider(&appconfig.AppConfig{}); err != nil {
		t.Fatalf("rejectMCUForProvider(no mcu) = %v, want nil", err)
	}
	err := rejectMCUForProvider(&appconfig.AppConfig{MCU: &appconfig.MCUConfig{Sketch: "sketch"}})
	if err == nil || !strings.Contains(err.Error(), "only Arduino UNO Q") {
		t.Fatalf("rejectMCUForProvider(mcu) = %v, want the only-UNO-Q error", err)
	}
}

func TestRunWithAgent_HostCheckBeforeRegistration(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"}, nil)
	checkArduinoHostFn = func(context.Context) error { return errors.New("needs Rosetta 2") }
	registered := countRegistrations(t)
	err := runWithAgent(context.Background(), h.conn, h.cwd, h.cfg, runOptions{})
	if err == nil || !strings.Contains(err.Error(), "Rosetta") {
		t.Fatalf("runWithAgent() = %v, want the host error", err)
	}
	if *registered != 0 || h.compiles != 0 {
		t.Fatalf("registrations %d, compiles %d; want 0 and 0", *registered, h.compiles)
	}
}

func TestBuildMCU_CompilesWithoutADevice(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", nil, nil)
	if err := buildMCU(context.Background(), h.cwd, h.cfg); err != nil {
		t.Fatalf("buildMCU: %v", err)
	}
	if h.compiles != 1 || h.version.calls != 0 || h.mcu.calls != 0 {
		t.Fatalf("compiles %d, version calls %d, flash calls %d; want one compile and no device call", h.compiles, h.version.calls, h.mcu.calls)
	}
	h.cfg.MCU = nil
	if err := buildMCU(context.Background(), h.cwd, h.cfg); err != nil || h.compiles != 1 {
		t.Fatalf("buildMCU without mcu = %v, compiles %d; want nothing", err, h.compiles)
	}
}

func TestPrepareMCUSketch_AppLabProfileBuildsFromACopyWithNotices(t *testing.T) {
	h := newMCUHarness(t, "arduino-uno-q", []string{"mcu-flash:zephyr-1"}, nil)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	yaml := "profiles:\n  default:\n    platforms:\n      - platform: arduino:zephyr\ndefault_profile: default\n"
	if err := os.WriteFile(filepath.Join(h.cwd, "sketch", "sketch.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	var sketch *mcuSketch
	var err error
	out := captureStderr(t, func() { sketch, err = prepareMCUSketch(context.Background(), h.cwd, h.cfg) })
	if err != nil {
		t.Fatalf("prepareMCUSketch: %v", err)
	}
	if sketch.buildDir == sketch.sourceDir || sketch.core != arduinotoolchain.DefaultCore || sketch.layout != "zephyr-1" {
		t.Fatalf("sketch = %+v, want a staged copy on the default core", sketch)
	}
	for _, want := range []string{"names no board", "does not pin arduino:zephyr", `no "arduino-bridge" entitlement`} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks the notice %q", out, want)
		}
	}
}

func TestCompileMCUSketch_ErrorsNameTheProjectFiles(t *testing.T) {
	sketch := &mcuSketch{sourceDir: "/proj/sketch", buildDir: "/cache/arduino/sketches/ab/sketch", profile: "default"}
	orig := compileSketchFn
	t.Cleanup(func() { compileSketchFn = orig })
	compileSketchFn = func(context.Context, string, string, io.Writer) (*arduinotoolchain.Artifacts, error) {
		return nil, errors.New("the sketch does not compile:\n  /cache/arduino/sketches/ab/sketch/sketch.ino:3:1: error: expected ';'")
	}
	_, err := compileMCUSketch(context.Background(), &appconfig.AppConfig{MCU: &appconfig.MCUConfig{Sketch: "sketch"}}, sketch)
	if err == nil || !strings.Contains(err.Error(), "/proj/sketch/sketch.ino:3:1: error") || strings.Contains(err.Error(), "/cache/") {
		t.Fatalf("err = %v, want the project's path", err)
	}
}

func TestInvalidAppConfigError_UnknownTypePointsAtTheCLIVersion(t *testing.T) {
	err := invalidAppConfigError(errors.New(`entitlement[0]: unknown type "arduino-bridge"`))
	if ErrorClass(err) != "config_invalid" || !strings.Contains(strings.Join(NextSteps(err), "\n"), "wendy update") {
		t.Fatalf("err = %v, steps %q; want config_invalid with the update hint", err, NextSteps(err))
	}
	if steps := NextSteps(invalidAppConfigError(errors.New("appId is required"))); len(steps) != 0 {
		t.Fatalf("steps for another error = %q, want none", steps)
	}
}
