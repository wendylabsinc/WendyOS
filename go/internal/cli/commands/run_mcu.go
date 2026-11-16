package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/arduinotoolchain"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/terminaloutput"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

const (
	mcuFeaturePrefix = "mcu-flash:"
	// mcuOwnerPrefix names the app whose sketch is on the chip now.
	mcuOwnerPrefix = "mcu-sketch-owner:"
	// mcuContentUnknownFeature: an interrupted flash may have damaged the owner's sketch.
	mcuContentUnknownFeature = "mcu-sketch-unknown"
	mcuChunkSize             = 1 << 20
	mcuLogTailLines          = 20
)

var errMCUNeedsUnoQ = commandErrorf(errProjectTargetMismatch, `this project has a microcontroller sketch (wendy.json "mcu"); only Arduino UNO Q supports it`)

// compileSketchFn compiles into a per-sketch build dir so the next run is incremental.
var compileSketchFn = func(ctx context.Context, sketchDir, profile string, out io.Writer) (*arduinotoolchain.Artifacts, error) {
	cli, err := arduinotoolchain.EnsureCLI(ctx, out)
	if err != nil {
		return nil, err
	}
	buildDir, err := mcuBuildDir(sketchDir)
	if err != nil {
		return nil, err
	}
	return arduinotoolchain.Compile(ctx, cli, sketchDir, profile, buildDir, out, out)
}

func mcuBuildDir(sketchDir string) (string, error) {
	root, err := config.CacheDir()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(sketchDir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(root, "arduino", "build", hex.EncodeToString(sum[:8])), nil
}

// registerCloudAppsFn lets tests see whether a run reached cloud registration.
var registerCloudAppsFn = registerCloudApps

var checkArduinoHostFn = arduinotoolchain.CheckHost

// mcuPlan is what preflightMCU checked, for deployMCU to build and flash.
type mcuPlan struct {
	version *agentpb.GetAgentVersionResponse
	sketch  *mcuSketch
}

// mcuSketch is the project's sketch, checked on this machine alone.
type mcuSketch struct {
	sourceDir string // the project's folder
	buildDir  string // sourceDir, or a staged copy when sketch.yaml needed defaults
	profile   string
	core      string
	layout    string
}

// prepareMCUSketch checks the sketch without a device: its profile, its core
// and whether this machine can compile it.
func prepareMCUSketch(ctx context.Context, cwd string, appCfg *appconfig.AppConfig) (*mcuSketch, error) {
	sourceDir := filepath.Join(cwd, filepath.FromSlash(appCfg.MCU.Sketch))
	prepared, err := arduinotoolchain.PrepareSketch(sourceDir, appCfg.MCU.Profile)
	if err != nil {
		return nil, commandErrorf(errConfigInvalid, "microcontroller sketch: %v", err)
	}
	layout, err := arduinotoolchain.LayoutForCore(prepared.Core)
	if err != nil {
		return nil, commandErrorf(errConfigInvalid, "microcontroller sketch: %v", err)
	}
	if err := checkArduinoHostFn(ctx); err != nil {
		return nil, err
	}
	for _, n := range prepared.Notices {
		cliNotice("Notice: %s", n)
	}
	if !appCfg.HasEntitlement(appconfig.EntitlementArduinoBridge) && len(appCfg.Services) == 0 {
		cliNotice(`Notice: this app has a sketch but no "arduino-bridge" entitlement, so it cannot talk to the sketch.`)
	}
	return &mcuSketch{sourceDir: sourceDir, buildDir: prepared.Dir, profile: prepared.Profile, core: prepared.Core, layout: layout}, nil
}

// compileMCUSketch compiles the prepared sketch. Compiler errors name the
// project's files, not the staged copy.
func compileMCUSketch(ctx context.Context, appCfg *appconfig.AppConfig, sketch *mcuSketch) (*arduinotoolchain.Artifacts, error) {
	cliLogln("Compiling microcontroller sketch %s...", appCfg.MCU.Sketch)
	artifacts, err := compileSketchFn(ctx, sketch.buildDir, sketch.profile, terminaloutput.Stderr)
	if err != nil {
		return nil, commandErrorf(errBuildFailed, "microcontroller sketch: %s", strings.ReplaceAll(err.Error(), sketch.buildDir, sketch.sourceDir))
	}
	return artifacts, nil
}

// buildMCU is `wendy build` for the sketch: compile only, no device needed.
func buildMCU(ctx context.Context, cwd string, appCfg *appconfig.AppConfig) error {
	if appCfg == nil || appCfg.MCU == nil {
		return nil
	}
	sketch, err := prepareMCUSketch(ctx, cwd, appCfg)
	if err != nil {
		return err
	}
	_, err = compileMCUSketch(ctx, appCfg, sketch)
	return err
}

// preflightMCU refuses, before anything leaves this machine or is downloaded,
// a sketch the device cannot flash or a project the agent would refuse after
// the flash. It returns nil without wendy.json "mcu".
func preflightMCU(ctx context.Context, conn *grpcclient.AgentConnection, cwd string, appCfg *appconfig.AppConfig, opts runOptions) (*mcuPlan, error) {
	if appCfg == nil || appCfg.MCU == nil {
		return nil, nil
	}
	if len(opts.fleetDevices) > 0 {
		return nil, commandErrorf(errProjectTargetMismatch, "a project with a microcontroller sketch cannot be deployed to several devices in one run (%s): only the first would get the sketch; run it per device", strings.Join(opts.fleetDevices, ", "))
	}
	version, err := agentVersionForRun(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("querying the device before flashing its microcontroller: %w", err)
	}
	if err := checkMCUBoard(version); err != nil {
		return nil, err
	}
	sketch, err := prepareMCUSketch(ctx, cwd, appCfg)
	if err != nil {
		return nil, err
	}
	if !mcuLayoutSupported(version, sketch.layout) {
		return nil, errMCUCoreUnsupported(sketch.core)
	}
	cfgs := []*appconfig.AppConfig{appCfg}
	if len(appCfg.Services) > 0 {
		services, err := resolveServiceSubset(appCfg.Services, opts.service)
		if err != nil {
			return nil, err
		}
		for _, svc := range services {
			cfgs = append(cfgs, &appconfig.AppConfig{Entitlements: svc.Entitlements})
		}
	}
	if err := checkAgentEntitlements(version, cfgs...); err != nil {
		return nil, err
	}
	return &mcuPlan{version: version, sketch: sketch}, nil
}

// rejectMCUForProvider stops a sketch on targets without a Wendy agent, which
// would otherwise deploy the rest of the project and drop it silently.
func rejectMCUForProvider(appCfg *appconfig.AppConfig) error {
	if appCfg == nil || appCfg.MCU == nil {
		return nil
	}
	return errMCUNeedsUnoQ
}

func errMCUCoreUnsupported(core string) error {
	return commandErrorf(errProjectTargetMismatch, "this WendyOS can't flash Arduino core %s; update WendyOS", core)
}

// mcuSourceHash identifies what a compile would build: the sketch's files and profile.
func mcuSourceHash(sketchDir, profile string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", profile)
	err := filepath.WalkDir(sketchDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		sum, err := sha256File(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(sketchDir, path)
		fmt.Fprintf(h, "%s %s\n", sum, filepath.ToSlash(rel))
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

// deployMCU compiles and flashes the sketch preflightMCU checked. A watch
// session skips both, and the MCU reset, while the sketch is unchanged.
func deployMCU(ctx context.Context, conn *grpcclient.AgentConnection, plan *mcuPlan, appCfg *appconfig.AppConfig, opts runOptions) error {
	if plan == nil {
		return nil
	}
	// Keyed by device, so a board swapped in at the same address is flashed.
	key := "mcu:" + deviceFingerprintKey(plan.version) + "/" + appCfg.AppID
	source := ""
	if opts.isWatch() {
		var err error
		if source, err = mcuSourceHash(plan.sketch.sourceDir, plan.sketch.profile); err != nil {
			return fmt.Errorf("microcontroller sketch: %w", err)
		}
		if opts.watchState.matches(key, source) && mcuOwnerNowFn(ctx, conn) == appCfg.AppID {
			cliLogln("Microcontroller: unchanged since the last flash")
			return nil
		}
	}
	artifacts, err := compileMCUSketch(ctx, appCfg, plan.sketch)
	if err != nil {
		return err
	}
	if !mcuLayoutSupported(plan.version, artifacts.Layout) {
		return errMCUCoreUnsupported(artifacts.CoreVersion)
	}
	spec, err := mcuSpec(appCfg.AppID, artifacts.Layout, artifacts.LoaderPath, artifacts.SketchPath)
	if err != nil {
		return err
	}
	// A failed or cancelled flash may still have changed the MCU: forget the last one first.
	opts.watchState.record(key, "")
	cliLogln("Flashing the microcontroller...")
	done, err := flashMCU(ctx, conn.McuService, spec, artifacts.LoaderPath, artifacts.SketchPath, terminaloutput.Stderr)
	if err != nil {
		return err
	}
	if source != "" {
		opts.watchState.record(key, source)
	}
	cliLogln("%s", mcuSummary(done, appCfg.AppID))
	if !done.GetRouterActive() {
		cliNotice("Warning: Arduino router is not running; the sketch starts once it is")
	}
	return nil
}

func checkMCUBoard(v *agentpb.GetAgentVersionResponse) error {
	for _, f := range v.GetFeatureset() {
		if strings.HasPrefix(f, mcuFeaturePrefix) {
			return nil
		}
	}
	if v.GetDeviceType() == arduinoUnoQDeviceType {
		return commandErrorf(errProjectTargetMismatch, "this board's WendyOS or agent can't flash the microcontroller yet; update both with 'wendy device update'")
	}
	return errMCUNeedsUnoQ
}

func mcuLayoutSupported(v *agentpb.GetAgentVersionResponse, layout string) bool {
	return agentVersionHasFeature(v, mcuFeaturePrefix+layout)
}

func mcuSpec(appID, layout, loaderPath, sketchPath string) (*agentpbv2.ArduinoSketchSpec, error) {
	loader, err := mcuImage(loaderPath)
	if err != nil {
		return nil, err
	}
	sketch, err := mcuImage(sketchPath)
	if err != nil {
		return nil, err
	}
	return &agentpbv2.ArduinoSketchSpec{Layout: layout, Loader: loader, Sketch: sketch, AppId: appID}, nil
}

func mcuImage(path string) (*agentpbv2.ArduinoImage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("microcontroller image: %w", err)
	}
	sum, err := sha256File(path)
	if err != nil {
		return nil, fmt.Errorf("hashing %s: %w", path, err)
	}
	return &agentpbv2.ArduinoImage{Size: uint64(info.Size()), Sha256: sum}, nil
}

// flashMCU streams the spec, then the loader and sketch bytes back to back,
// and returns the agent's result. On failure it prints the helper's last lines.
func flashMCU(ctx context.Context, client agentpbv2.WendyArduinoMcuServiceClient, spec *agentpbv2.ArduinoSketchSpec, loaderPath, sketchPath string, out io.Writer) (*agentpbv2.FlashSketchResponse_Completed, error) {
	stream, err := client.FlashSketch(ctx)
	if err != nil {
		return nil, fmt.Errorf("starting the microcontroller flash: %w", err)
	}
	if err := stream.Send(&agentpbv2.FlashSketchRequest{RequestType: &agentpbv2.FlashSketchRequest_Spec{Spec: spec}}); err != nil {
		return nil, fmt.Errorf("sending the microcontroller flash spec: %w", err)
	}
	if err := sendMCUChunks(stream, loaderPath, sketchPath); err != nil {
		return nil, err
	}
	if err := stream.CloseSend(); err != nil {
		return nil, fmt.Errorf("closing the microcontroller upload: %w", err)
	}

	var tail []string
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("microcontroller flash ended without a result")
		}
		if err != nil {
			return nil, fmt.Errorf("microcontroller flash: %w", err)
		}
		switch r := resp.GetResponseType().(type) {
		case *agentpbv2.FlashSketchResponse_Progress_:
			if line := r.Progress.GetLogLine(); line != "" {
				tail = append(tail, line)
				if len(tail) > mcuLogTailLines {
					tail = tail[1:]
				}
			}
		case *agentpbv2.FlashSketchResponse_Failed_:
			for _, line := range tail {
				fmt.Fprintln(out, "  "+line)
			}
			return nil, fmt.Errorf("flashing the microcontroller: %s", r.Failed.GetErrorMessage())
		case *agentpbv2.FlashSketchResponse_Completed_:
			return r.Completed, nil
		}
	}
}

func sendMCUChunks(stream agentpbv2.WendyArduinoMcuService_FlashSketchClient, paths ...string) error {
	var readers []io.Reader
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("opening %s: %w", p, err)
		}
		defer f.Close()
		readers = append(readers, f)
	}
	r := io.MultiReader(readers...)
	for {
		// A fresh buffer per chunk: gRPC may read a sent message after Send returns.
		buf := make([]byte, mcuChunkSize)
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if serr := stream.Send(&agentpbv2.FlashSketchRequest{RequestType: &agentpbv2.FlashSketchRequest_Chunk_{
				Chunk: &agentpbv2.FlashSketchRequest_Chunk{Data: buf[:n]}}}); serr != nil {
				// The agent closed early; its reason arrives on Recv.
				return nil
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the microcontroller images: %w", err)
		}
	}
}

func mcuSummary(c *agentpbv2.FlashSketchResponse_Completed, appID string) string {
	state := func(written bool) string {
		if written {
			return "written"
		}
		return "unchanged"
	}
	s := fmt.Sprintf("Microcontroller: sketch %s, loader %s", state(c.GetSketchWritten()), state(c.GetLoaderWritten()))
	if prev := c.GetPreviousAppId(); prev != "" && prev != appID {
		s += fmt.Sprintf("; replaced the sketch of %s and stopped it (one sketch fits on the board)", prev)
	}
	return s
}

// invalidAppConfigError explains a wendy.json this CLI refuses. An entitlement
// type it does not know is usually a newer project on an older CLI.
func invalidAppConfigError(err error) error {
	out := commandErrorf(errConfigInvalid, "invalid wendy.json: %w", err)
	if strings.Contains(err.Error(), "unknown type") {
		return withNextSteps(out, "If the project uses a newer entitlement, this wendy ("+version.Version+") is too old for it: run `wendy update`.")
	}
	return out
}

// mcuOwnerNowFn asks the device, not the session's cached answer: another
// deploy or a remove may have changed the chip since this session flashed it.
var mcuOwnerNowFn = func(ctx context.Context, conn *grpcclient.AgentConnection) string {
	v, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return ""
	}
	if slices.Contains(v.GetFeatureset(), mcuContentUnknownFeature) {
		return ""
	}
	return mcuSketchOwner(v)
}

// mcuSketchOwner returns the app whose sketch is on the device's chip, or "".
func mcuSketchOwner(v *agentpb.GetAgentVersionResponse) string {
	for _, f := range v.GetFeatureset() {
		if owner, ok := strings.CutPrefix(f, mcuOwnerPrefix); ok {
			return owner
		}
	}
	return ""
}
