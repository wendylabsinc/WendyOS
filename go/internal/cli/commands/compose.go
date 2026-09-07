package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/distribution/reference"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/stagefile"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// normalizeImageRef canonicalises a Docker short reference (e.g.
// "python:3.11-slim", "nginx") to a fully-qualified form
// ("docker.io/library/python:3.11-slim") that the agent's containerd
// reference parser accepts. References that already include a registry,
// digest, or tag pass through unchanged. When parsing fails (malformed
// reference), the original input is returned and the agent surfaces the
// resulting error.
func normalizeImageRef(ref string) string {
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(ref))
	if err != nil {
		return ref
	}
	return reference.TagNameOnly(named).String()
}

func imageRefContentPinned(ref string) bool {
	parsed, err := reference.ParseAnyReference(strings.TrimSpace(ref))
	if err != nil {
		return false
	}
	_, ok := parsed.(reference.Digested)
	return ok
}

func composeServiceWatchHash(imageIdentity string, cfg *appconfig.AppConfig, cmd string, userArgs []string, restartPolicy *agentpb.RestartPolicy, env []string) (string, error) {
	return watchDesiredHash(struct {
		ImageIdentity string
		Config        *appconfig.AppConfig
		Cmd           string
		UserArgs      []string
		RestartPolicy *agentpb.RestartPolicy
		Env           []string
	}{imageIdentity, cfg, cmd, userArgs, restartPolicy, env})
}

// planComposeBuildSkips returns built-image services whose complete desired
// state matches the last successful device preparation, whose container is
// still known to the device, and whose exact uncompressed image layers remain
// in its content store. Every check fails closed: a stale local fingerprint or
// a device-side GC can only cause an unnecessary preparation, never stale code.
func planComposeBuildSkips(ctx context.Context, conn *grpcclient.AgentConnection, deviceKey string, desiredHashes map[string]string, svcCfgs map[string]*appconfig.AppConfig, states map[string]agentpb.AppRunningState) map[string]bool {
	skip := map[string]bool{}
	if os.Getenv("WENDY_PUSH_SKIP") == "0" {
		return skip
	}
	for name, desiredHash := range desiredHashes {
		cfg := svcCfgs[name]
		if cfg == nil || desiredHash == "" {
			continue
		}
		if _, present := states[strings.ToLower(cfg.ContainerName())]; !present {
			continue
		}
		fp, ok := loadDeployFingerprint(serviceFingerprintKey(cfg.AppID, name), deviceKey)
		if !ok || fp.InputHash != desiredHash || !deviceHasAllLayers(ctx, conn, fp.LayerDiffIDs) {
			continue
		}
		skip[name] = true
	}
	return skip
}

// composeConfig is a minimal representation of a docker-compose file.
type composeConfig struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Image       string    `yaml:"image"`
	Build       yaml.Node `yaml:"build"` // string or build object
	Command     yaml.Node `yaml:"command"`
	Environment yaml.Node `yaml:"environment"` // map or list
	Ports       []string  `yaml:"ports"`
	Volumes     []string  `yaml:"volumes"`
	DependsOn   yaml.Node `yaml:"depends_on"` // list or map
	Restart     string    `yaml:"restart"`
	NetworkMode string    `yaml:"network_mode"`

	// XWendy carries Wendy-specific service extensions (readiness, hooks).
	XWendy yaml.Node `yaml:"x-wendy"`

	// Captured only for warning purposes — not used in deployment.
	Devices     yaml.Node `yaml:"devices"`
	Privileged  yaml.Node `yaml:"privileged"`
	CapAdd      yaml.Node `yaml:"cap_add"`
	SecurityOpt yaml.Node `yaml:"security_opt"`
	IPC         string    `yaml:"ipc"`
	PID         string    `yaml:"pid"`
	ShmSize     string    `yaml:"shm_size"`
	HealthCheck yaml.Node `yaml:"healthcheck"`
	Profiles    yaml.Node `yaml:"profiles"`
	Secrets     yaml.Node `yaml:"secrets"`
	ExtraHosts  yaml.Node `yaml:"extra_hosts"`
}

// composeBuildJob is all per-service state needed after Compose parsing and
// Stagefile compilation have completed. Keeping preparation out of the build
// goroutines avoids duplicate writes when several services intentionally share
// one context and build file.
type composeBuildJob struct {
	contextDir string
	dockerfile string
	repo       string
	buildArgs  map[string]string
}

// imageBuildProgressWriter adapts the common chunk/preparation progress
// callbacks and captures the prepared image identity for both Compose and
// wendy.json multi-service schedulers.
type imageBuildProgressWriter struct {
	io.Writer
	emit          func(tui.BuildStepEvent)
	reportContent func([]string)
	progressMu    sync.Mutex
	uploadStarted bool
}

func (w *imageBuildProgressWriter) emitEvent(e tui.BuildStepEvent) {
	if w.emit != nil {
		w.emit(e)
	}
}

// ReportImageContent records the uncompressed identities of the image that was
// prepared on the device. Keeping this on the same writer used for chunk
// progress lets both service schedulers persist a fail-closed deploy fingerprint
// without coupling their generic build function signatures to OCI internals.
func (w *imageBuildProgressWriter) ReportImageContent(diffIDs []string) {
	if w.reportContent != nil {
		w.reportContent(append([]string(nil), diffIDs...))
	}
}

func (w *imageBuildProgressWriter) ReportChunkTransfer(current, total int64, rate float64) {
	w.progressMu.Lock()
	defer w.progressMu.Unlock()
	w.uploadStarted = true
	w.emitEvent(tui.BuildStepEvent{
		ID:      "chunk-upload",
		Kind:    tui.BuildVertexExport,
		Display: "uploading missing chunks",
		Status:  tui.BuildStepRunning,
		Bytes:   tui.ByteProgress{Current: current, Total: total, Rate: rate},
	})
}

func (w *imageBuildProgressWriter) ReportChunkIndex(current, total int64, rate float64, done bool) {
	status := tui.BuildStepRunning
	if done {
		status = tui.BuildStepDone
	}
	w.progressMu.Lock()
	defer w.progressMu.Unlock()
	// Indexing and upload intentionally overlap across layers. The Compose TUI
	// has one detail line per service, so once upload begins it is the more useful
	// signal and late index updates must not repeatedly replace it. Still forward
	// the terminal event so consumers can close the synthetic index step.
	if w.uploadStarted && status == tui.BuildStepRunning {
		return
	}
	w.emitEvent(tui.BuildStepEvent{
		ID:      "chunk-index",
		Kind:    tui.BuildVertexSetup,
		Display: "indexing changed layer content",
		Status:  status,
		Bytes:   tui.ByteProgress{Current: current, Total: total, Rate: rate},
	})
}

func (w *imageBuildProgressWriter) ReportRegistryFallback(reason error) {
	detail := "chunk preparation unavailable"
	if reason != nil {
		detail = fmt.Sprintf("chunk preparation unavailable: %v", reason)
	}
	w.emitEvent(tui.BuildStepEvent{
		ID:      "registry-fallback",
		Kind:    tui.BuildVertexExport,
		Display: "uploading image via registry fallback",
		Status:  tui.BuildStepRunning,
		Detail:  detail,
	})
}

func (w *imageBuildProgressWriter) ReportImagePreparation() {
	w.emitEvent(tui.BuildStepEvent{
		ID:      "image-prepare",
		Kind:    tui.BuildVertexExport,
		Display: "preparing image on device",
		Status:  tui.BuildStepRunning,
	})
}

func (w *imageBuildProgressWriter) ReportImagePreparationQueued() {
	w.emitEvent(tui.BuildStepEvent{
		ID:      "image-prepare-queue",
		Kind:    tui.BuildVertexExport,
		Display: "waiting for device preparation slot",
		Status:  tui.BuildStepRunning,
	})
}

// buildComposeServiceImage is replaceable in tests so the parallel scheduler
// can be exercised without Docker or a device registry.
var buildComposeServiceImage = buildAndPrepareComposeImageForAgent

const maxConcurrentComposePrepares = 2

type composePrepareLimiterKey struct{}

func withComposePrepareLimiter(ctx context.Context) context.Context {
	return context.WithValue(ctx, composePrepareLimiterKey{}, make(chan struct{}, maxConcurrentComposePrepares))
}

func acquireComposePrepare(ctx context.Context) (func(), error) {
	limiter, _ := ctx.Value(composePrepareLimiterKey{}).(chan struct{})
	if limiter == nil {
		return func() {}, nil
	}
	select {
	case limiter <- struct{}{}:
		return func() { <-limiter }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// buildComposeServicesParallel runs independent Compose image jobs
// concurrently. By default each job builds a persistent OCI layout and sends
// only content chunks the device does not already have; --chunking=off keeps
// the registry-push path for compatibility.
func buildComposeServicesParallel(ctx context.Context, conn *grpcclient.AgentConnection, regPort int, agentOS, builder, platform, chunking string, jobs map[string]composeBuildJob, maxConcurrency int, quietBuild bool) (map[string]error, map[string][]string, error) {
	buildCtx, cancelBuild := context.WithCancel(ctx)
	defer cancelBuild()
	buildCtx = withComposePrepareLimiter(buildCtx)

	names := make([]string, 0, len(jobs))
	for name := range jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return map[string]error{}, map[string][]string{}, nil
	}

	concurrency := resolveBuildConcurrency(len(names), maxConcurrency)
	if !quietBuild && maxConcurrency > 0 && concurrency < len(names) {
		cliLogln("Building up to %d service(s) at a time (--max-concurrency).", concurrency)
	}

	type result struct {
		name         string
		err          error
		log          string
		layerDiffIDs []string
	}
	results := make(chan result, len(names))
	sem := make(chan struct{}, concurrency)

	var prog *tea.Program
	if !quietBuild && isInteractiveTerminal() {
		prog = tui.NewProgressProgram(tui.NewMultiSpinner(fmt.Sprintf("Building %d Compose service(s)...", len(names)), names))
	}

	var wg sync.WaitGroup
	for _, name := range names {
		job := jobs[name]
		wg.Add(1)
		go func(name string, job composeBuildJob) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-buildCtx.Done():
				results <- result{name: name, err: buildCtx.Err()}
				return
			}
			defer func() { <-sem }()
			if err := buildCtx.Err(); err != nil {
				results <- result{name: name, err: err}
				return
			}

			if prog != nil {
				prog.Send(tui.MultiSpinnerStartMsg{Name: name})
			} else if !quietBuild {
				cliLogln("Building service %s for %s...", name, tui.Value(platform))
			}

			start := time.Now()
			var logBuf bytes.Buffer
			var buildOut io.Writer = os.Stdout
			var logOut io.Writer = os.Stderr
			var layerDiffIDs []string
			var tally func() tui.BuildTally = func() tui.BuildTally { return tui.BuildTally{} }
			var emit func(tui.BuildStepEvent)
			if prog != nil {
				var getTally func() tui.BuildTally
				emit, getTally = newServiceProgressEmitter(prog, name)
				tally = getTally
				buildOut = io.MultiWriter(tui.NewBuildParser(emit), &logBuf)
				logOut = &logBuf
			} else if quietBuild {
				buildOut = &logBuf
				logOut = &logBuf
			}
			buildOut = &imageBuildProgressWriter{
				Writer: buildOut,
				emit:   emit,
				reportContent: func(ids []string) {
					layerDiffIDs = ids
				},
			}

			// The repo also scopes buildx's local cache export. Concurrent writers
			// must not share that directory; BuildKit cache mounts inside the
			// builder (APT, pip, etc.) remain shared by their explicit mount IDs.
			buildImage := buildComposeServiceImage
			if chunking == chunkingOff {
				buildImage = buildAndPushImageForAgent
			} else if chunking == chunkingForce {
				buildImage = buildAndPrepareComposeImageForAgentForce
			}
			err := buildImage(buildCtx, conn, regPort, agentOS, builder, job.contextDir, job.repo, platform, job.dockerfile, job.buildArgs, job.repo, buildOut, logOut)
			dur := time.Since(start)
			if prog != nil {
				t := tally()
				prog.Send(tui.MultiSpinnerDoneMsg{Name: name, Err: err, Dur: dur, Cached: t.Cached, Rebuilt: t.Rebuilt})
			} else if err != nil {
				if buildCtx.Err() == nil {
					if !quietBuild {
						cliLogln("Service %s build failed: %v", name, err)
					}
				}
			} else if !quietBuild {
				cliLogln("Service %s built (%s).", name, dur.Round(time.Millisecond))
			}
			results <- result{name: name, err: err, log: logBuf.String(), layerDiffIDs: layerDiffIDs}
		}(name, job)
	}

	go func() {
		wg.Wait()
		close(results)
		if prog != nil {
			prog.Send(tui.MultiSpinnerAllDoneMsg{})
		}
	}()

	var progressErr error
	if prog != nil {
		final, runErr := prog.Run()
		if runErr != nil {
			cancelBuild()
			progressErr = fmt.Errorf("compose build progress TUI: %w", runErr)
		} else if fm, ok := final.(tui.MultiSpinnerModel); !ok {
			cancelBuild()
			progressErr = fmt.Errorf("compose build progress TUI: unexpected final model %T", final)
		} else if errors.Is(fm.Err(), tui.ErrCancelled) {
			cancelBuild()
			progressErr = ErrUserCancelled
		}
	}

	failed := map[string]error{}
	content := map[string][]string{}
	for r := range results {
		if r.err == nil {
			if len(r.layerDiffIDs) > 0 {
				content[r.name] = r.layerDiffIDs
			}
			continue
		}
		failed[r.name] = r.err
		if progressErr == nil && buildCtx.Err() == nil && r.log != "" && !isRegistryUnavailable(r.err) {
			fmt.Fprintf(os.Stderr, "\n[%s] build log:\n%s", r.name, r.log)
		}
	}
	if progressErr != nil {
		return nil, nil, progressErr
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	return failed, content, nil
}

// unsupportedComposeWarnings returns field names from svc that Wendy does not
// honour during deployment. The caller should print these to the user.
func unsupportedComposeWarnings(svc composeService) []string {
	type check struct {
		name  string
		empty bool
	}
	checks := []check{
		{"devices", svc.Devices.IsZero()},
		{"privileged", svc.Privileged.IsZero()},
		{"cap_add", svc.CapAdd.IsZero()},
		{"security_opt", svc.SecurityOpt.IsZero()},
		{"ipc", svc.IPC == ""},
		{"pid", svc.PID == ""},
		{"shm_size", svc.ShmSize == ""},
		{"healthcheck", svc.HealthCheck.IsZero()},
		{"profiles", svc.Profiles.IsZero()},
		{"secrets", svc.Secrets.IsZero()},
		{"extra_hosts", svc.ExtraHosts.IsZero()},
	}
	var warned []string
	for _, c := range checks {
		if !c.empty {
			warned = append(warned, c.name)
		}
	}
	return warned
}

type composeBuildConfig struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile"`
	Args       map[string]string `yaml:"args"`
}

// parseComposeFile reads and parses a docker-compose file.
func parseComposeFile(dir string) (*composeConfig, string, error) {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg composeConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, "", fmt.Errorf("parsing %s: %w", name, err)
		}
		return &cfg, name, nil
	}
	return nil, "", fmt.Errorf("no docker-compose file found in %s", dir)
}

// defaultComposeBuildFile mirrors single-service build-file precedence for a
// compose service that doesn't name a dockerfile explicitly: a Stagefile in
// the build context wins over the conventional "Dockerfile".
func defaultComposeBuildFile(ctxDir string) string {
	// SourceNames is canonical-first, so a context carrying build.stagefile.yaml
	// resolves to it. A context holding ONLY variants has no conventional default;
	// leaving it to "Dockerfile" would silently ignore the Stagefiles that are
	// plainly there, so the first variant wins and the service can name another
	// with an explicit `dockerfile:`.
	if names := stagefile.SourceNames(ctxDir); len(names) > 0 {
		return names[0]
	}
	return "Dockerfile"
}

// composeBuildContext returns the build context dir and Dockerfile for a service.
// Returns ("", "", nil) when the service uses a pre-built image.
func composeBuildContext(svc composeService, projectDir string) (ctxDir, dockerfile string, buildArgs map[string]string, err error) {
	if svc.Build.IsZero() {
		return "", "", nil, nil
	}

	switch svc.Build.Kind {
	case yaml.ScalarNode:
		// build: ./path
		ctxDir = filepath.Join(projectDir, svc.Build.Value)
		return ctxDir, defaultComposeBuildFile(ctxDir), nil, nil

	case yaml.MappingNode:
		var bc composeBuildConfig
		if err := svc.Build.Decode(&bc); err != nil {
			return "", "", nil, fmt.Errorf("decoding build config: %w", err)
		}
		ctxDir = projectDir
		if bc.Context != "" {
			ctxDir = filepath.Join(projectDir, bc.Context)
		}
		df := defaultComposeBuildFile(ctxDir)
		if bc.Dockerfile != "" {
			if err := validateComposeDockerfileName(bc.Dockerfile); err != nil {
				return "", "", nil, fmt.Errorf("compose dockerfile: %w", err)
			}
			if _, err := confinedDockerfilePath(ctxDir, bc.Dockerfile); err != nil {
				return "", "", nil, fmt.Errorf("compose dockerfile: %w", err)
			}
			df = bc.Dockerfile
		}
		return ctxDir, df, bc.Args, nil
	}

	return "", "", nil, fmt.Errorf("unsupported build directive (yaml kind %d); expected a path string or a mapping", svc.Build.Kind)
}

// composeGPUArch is the GPU architecture every buildable service in a compose
// project compiles against. The device is asked once for the whole project,
// and only if some service actually declares a cuda: stage.
func composeGPUArch(ctx context.Context, projectDir string, cfg *composeConfig, conn *grpcclient.AgentConnection) string {
	var dirs []string
	for _, svc := range cfg.Services {
		ctxDir, _, _, err := composeBuildContext(svc, projectDir)
		if err != nil || ctxDir == "" {
			continue
		}
		dirs = append(dirs, ctxDir)
	}
	return resolveGPUArchForDirs(ctx, dirs, "", conn)
}

// composeStagefileOverride compiles every compose service whose build context
// carries a Stagefile and writes a docker-compose override file pointing those
// services at their compiled Dockerfile.generated — `docker compose build`
// itself knows nothing about build.stagefile.yaml. Returns "" when no service
// uses a Stagefile. The caller must invoke cleanup (when non-nil) after the
// compose build finishes.
func composeStagefileOverride(dir, gpuArch string, sfOpts ...stagefile.Option) (path string, cleanup func(), err error) {
	cfg, _, err := parseComposeFile(dir)
	if err != nil {
		return "", nil, err
	}
	companion, _, err := appconfig.LoadComposeCompanion(dir)
	if err != nil {
		return "", nil, fmt.Errorf("companion wendy.json: %w", err)
	}
	serviceConfigs, _, err := buildComposeServiceConfigs(strings.ToLower(filepath.Base(dir)), cfg, companion)
	if err != nil {
		return "", nil, err
	}
	overrides := map[string]any{}
	for name, svc := range cfg.Services {
		ctxDir, dockerfile, _, err := composeBuildContext(svc, dir)
		if err != nil {
			return "", nil, fmt.Errorf("service %s: %w", name, err)
		}
		if ctxDir == "" || !stagefile.IsSourceName(dockerfile) {
			continue
		}
		serviceOpts := append([]stagefile.Option{}, sfOpts...)
		serviceOpts = append(serviceOpts, frameworkStagefileOptions(serviceConfigs[name].Frameworks)...)
		compiled, err := prepareDockerBuildFile(ctxDir, dockerfile, gpuArch, serviceOpts...)
		if err != nil {
			return "", nil, fmt.Errorf("service %s: %w", name, err)
		}
		ctxRel, err := filepath.Rel(dir, ctxDir)
		if err != nil {
			ctxRel = ctxDir
		}
		overrides[name] = map[string]any{
			"build": map[string]string{"context": ctxRel, "dockerfile": compiled},
		}
	}
	if len(overrides) == 0 {
		return "", nil, nil
	}
	data, err := yaml.Marshal(map[string]any{"services": overrides})
	if err != nil {
		return "", nil, fmt.Errorf("marshaling compose override: %w", err)
	}
	f, err := os.CreateTemp("", "wendy-compose-stagefile-*.yml")
	if err != nil {
		return "", nil, fmt.Errorf("creating compose override: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("writing compose override: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("closing compose override: %w", err)
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// composeCommand returns the command for a service as a slice. Sequence form
// preserves each argv element verbatim. Scalar form is shell-split into argv
// tokens, matching docker-compose's documented behaviour.
func composeCommand(svc composeService) []string {
	if svc.Command.IsZero() {
		return nil
	}
	switch svc.Command.Kind {
	case yaml.ScalarNode:
		return shellSplit(svc.Command.Value)
	case yaml.SequenceNode:
		var parts []string
		_ = svc.Command.Decode(&parts)
		return parts
	}
	return nil
}

// composeArgv splits a service's command into a (cmd, extraArgs) pair suitable
// for CreateContainerRequest.Cmd / UserArgs. cmd is guaranteed to be a single
// shell-safe token (no whitespace) so the agent's strings.Fields(cmd) split is
// a no-op; the remaining argv tokens flow through UserArgs unchanged so
// arguments containing whitespace (e.g. a `-c <script>` body) are preserved.
func composeArgv(svc composeService) (string, []string) {
	parts := composeCommand(svc)
	if len(parts) == 0 {
		return "", nil
	}
	return parts[0], parts[1:]
}

// shellSplit performs minimal POSIX-style splitting on a string command:
// whitespace separates tokens, and pairs of single or double quotes group a
// run of characters into one token. Backslash escapes are not interpreted —
// callers needing those should use the YAML sequence form.
func shellSplit(s string) []string {
	var (
		tokens []string
		cur    strings.Builder
		quote  rune
		inTok  bool
	)
	flush := func() {
		if inTok {
			tokens = append(tokens, cur.String())
			cur.Reset()
			inTok = false
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
			inTok = true
		case r == '\'' || r == '"':
			quote = r
			inTok = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
			inTok = true
		}
	}
	flush()
	return tokens
}

// composeEnv returns environment variables for a service as KEY=VALUE strings.
// Mapping values may be strings, numbers, bools, or null (inherit from process env).
// Sequence entries may be "KEY=VALUE" or bare "KEY" (inherit from process env).
func composeEnv(svc composeService) []string {
	if svc.Environment.IsZero() {
		return nil
	}
	var result []string
	switch svc.Environment.Kind {
	case yaml.MappingNode:
		var m map[string]any
		if err := svc.Environment.Decode(&m); err == nil {
			for k, v := range m {
				if v == nil {
					if inherited, ok := os.LookupEnv(k); ok {
						result = append(result, k+"="+inherited)
					}
					continue
				}
				result = append(result, k+"="+fmt.Sprint(v))
			}
		}
		// Mapping nodes decode into a Go map, whose iteration order is
		// deliberately randomized. Keep this representation canonical so
		// build-input and watch desired-state hashes do not change between
		// otherwise identical deploy cycles.
		sort.Strings(result)
	case yaml.SequenceNode:
		var list []string
		if err := svc.Environment.Decode(&list); err == nil {
			for _, entry := range list {
				if strings.Contains(entry, "=") {
					result = append(result, entry)
					continue
				}
				if inherited, ok := os.LookupEnv(entry); ok {
					result = append(result, entry+"="+inherited)
				}
			}
		}
	}
	return result
}

// composeRestartPolicy converts a compose restart string to a proto RestartPolicy.
func composeRestartPolicy(restart string) *agentpb.RestartPolicy {
	switch restart {
	case "always", "unless-stopped":
		return &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED}
	case "on-failure":
		return &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_ON_FAILURE}
	case "no", "":
		return &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_NO}
	default:
		return &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_DEFAULT}
	}
}

// parseComposeVolume parses a docker-compose short volume spec into its
// (source, target, mode) parts. Handles three forms:
//
//	target                       — anonymous volume
//	source:target                — named volume or bind mount
//	source:target:options        — with options like "ro" or "rw"
//
// Windows-style absolute paths (e.g. "C:\\data:/in") are detected when the
// first segment is a single drive letter and merged back with the second.
// Returns empty strings when the input cannot be parsed.
func parseComposeVolume(v string) (source, target, mode string) {
	parts := strings.Split(v, ":")
	// Re-merge a leading "<letter>:<path>" Windows-style drive prefix.
	if len(parts) >= 2 && len(parts[0]) == 1 {
		c := parts[0][0]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			parts = append([]string{parts[0] + ":" + parts[1]}, parts[2:]...)
		}
	}
	switch len(parts) {
	case 1:
		return "", parts[0], ""
	case 2:
		return parts[0], parts[1], ""
	case 3:
		return parts[0], parts[1], parts[2]
	default:
		return "", "", ""
	}
}

// composeAppConfig builds an AppConfig for a compose service.
// It synthesises network entitlements from ports/network_mode and persist
// entitlements from named volumes.
//
// numServices is the total number of services in the compose file. When
// numServices > 1 and no companion wendy.json overrides the appID, all
// services are grouped under the project name (appID = projectName,
// ServiceName = serviceName). Single-service apps keep the legacy
// "projectName-serviceName" appID so existing deployments are unaffected.
func composeAppConfig(projectName, serviceName string, svc composeService, numServices int) *appconfig.AppConfig {
	var appID string
	var svcName string
	if numServices > 1 {
		appID = projectName
		svcName = serviceName
	} else {
		appID = projectName + "-" + serviceName
	}

	var entitlements []appconfig.Entitlement

	// Network entitlement.
	if svc.NetworkMode == "host" {
		entitlements = append(entitlements, appconfig.Entitlement{
			Type: appconfig.EntitlementNetwork,
			Mode: "host",
		})
	} else if len(svc.Ports) > 0 {
		var ports []appconfig.PortMapping
		for _, p := range svc.Ports {
			// Parse "host:container" or "container" format.
			parts := strings.SplitN(p, ":", 2)
			var pm appconfig.PortMapping
			if len(parts) == 2 {
				fmt.Sscanf(parts[0], "%d", &pm.Host)
				fmt.Sscanf(parts[1], "%d", &pm.Container)
			} else {
				fmt.Sscanf(parts[0], "%d", &pm.Container)
				pm.Host = pm.Container
			}
			if pm.Host > 0 && pm.Container > 0 {
				ports = append(ports, pm)
			}
		}
		if len(ports) > 0 {
			entitlements = append(entitlements, appconfig.Entitlement{
				Type:  appconfig.EntitlementNetwork,
				Ports: ports,
			})
		}
	}

	// Persist entitlements from named volumes (skip host-bind mounts ./path:).
	for _, v := range svc.Volumes {
		source, target, _ := parseComposeVolume(v)
		if source == "" || target == "" {
			continue
		}
		// Named volumes start with a letter; bind mounts start with . or /
		// (or, on Windows, a drive letter like "C:\\…" — already merged into source).
		if strings.HasPrefix(source, ".") || strings.HasPrefix(source, "/") {
			continue
		}
		if len(source) >= 2 && source[1] == ':' {
			// Windows-style absolute path bind mount.
			continue
		}
		entitlements = append(entitlements, appconfig.Entitlement{
			Type: appconfig.EntitlementPersist,
			Name: source,
			Path: target,
		})
	}

	return &appconfig.AppConfig{
		AppID:        appID,
		ServiceName:  svcName,
		Entitlements: entitlements,
	}
}

// composeWatchAppID returns the app identity used to filter session logs.
// Companion configs supply that identity directly; otherwise a single service
// uses its generated app ID and a service group uses the project name.
func composeWatchAppID(projectName string, companion *appconfig.AppConfig, svcCfgs map[string]*appconfig.AppConfig) string {
	if companion != nil {
		return companion.AppID
	}
	if len(svcCfgs) == 1 {
		for _, cfg := range svcCfgs {
			return cfg.AppID
		}
	}
	return projectName
}

// parseXWendy decodes a compose service's x-wendy extension into appconfig
// types via a YAML→map→JSON roundtrip, so wendy.json's exact camelCase keys
// (readiness.tcpSocket.port, timeoutSeconds, hooks.postStart.openURL/cli/agent)
// apply verbatim. yaml.v3 does not read json struct tags, hence the roundtrip.
func parseXWendy(node yaml.Node, serviceName string) (*appconfig.ReadinessConfig, *appconfig.HooksConfig, []string, error) {
	if node.IsZero() {
		return nil, nil, nil, nil
	}

	var raw map[string]any
	if err := node.Decode(&raw); err != nil {
		return nil, nil, nil, fmt.Errorf("service %q: x-wendy: %w", serviceName, err)
	}

	var unknown []string
	for k := range raw {
		if k != "readiness" && k != "hooks" {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	var warnings []string
	for _, k := range unknown {
		warnings = append(warnings, fmt.Sprintf("service %q: unknown x-wendy key %q (supported: readiness, hooks)", serviceName, k))
	}

	data, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("service %q: x-wendy: %w", serviceName, err)
	}
	var parsed struct {
		Readiness *appconfig.ReadinessConfig `json:"readiness"`
		Hooks     *appconfig.HooksConfig     `json:"hooks"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, nil, nil, fmt.Errorf("service %q: x-wendy: %w", serviceName, err)
	}

	if err := appconfig.ValidateReadiness(fmt.Sprintf("services.%s.x-wendy.readiness", serviceName), parsed.Readiness); err != nil {
		return nil, nil, nil, err
	}
	warnings = append(warnings, appconfig.LintHooks(parsed.Hooks, fmt.Sprintf("services.%s.x-wendy.hooks", serviceName))...)

	return parsed.Readiness, parsed.Hooks, warnings, nil
}

// composeCompanionWarnings returns warnings for service names declared in the
// companion wendy.json that have no matching service in the compose file.
// A nil companion produces no warnings.
func composeCompanionWarnings(companion *appconfig.AppConfig, composeCfg *composeConfig) []string {
	if companion == nil || len(companion.Services) == 0 {
		return nil
	}
	var warnings []string
	for name := range companion.Services {
		if _, ok := composeCfg.Services[name]; !ok {
			warnings = append(warnings, fmt.Sprintf("wendy.json: service %q is not defined in the compose file", name))
		}
	}
	sort.Strings(warnings)
	return warnings
}

// applyComposeCompanion merges Wendy-specific config from a companion wendy.json
// into an AppConfig synthesised from compose fields. companion may be nil (no
// wendy.json present).
//
// Merge rules:
//   - AppID and ServiceName are set from the companion so the agent creates the
//     container as "{appId}_{serviceName}" (WDY-878) and injects WENDY_APP_ID /
//     WENDY_HOSTNAME correctly.
//   - Top-level isolation and frameworks from the companion are applied to every service.
//   - Top-level entitlements from the companion are appended to every service's
//     entitlements (compose-derived network/persist entitlements are preserved).
//   - Per-service entitlements are appended on top of the shared ones.
//   - Per-service frameworks override the group-level frameworks for that service.
//   - Duplicate entitlements (same type+name+mode) are removed; the first
//     occurrence wins so compose-synthesised entitlements take precedence.
//   - Per-service readiness/hooks from the companion replace (not merge with)
//     any x-wendy-derived values for that service, wholesale per field.
//   - The companion's *top-level* readiness/hooks are deliberately never
//     copied onto per-service configs: they are an app-level fallback
//     reserved for a later stage, and copying hooks.postStart.agent
//     per-service would run it in every container.
func applyComposeCompanion(appCfg *appconfig.AppConfig, companion *appconfig.AppConfig, serviceName string) {
	if companion == nil {
		return
	}
	appCfg.AppID = companion.AppID
	appCfg.ServiceName = serviceName
	appCfg.Isolation = companion.Isolation
	appCfg.Frameworks = companion.Frameworks
	appCfg.Entitlements = append(appCfg.Entitlements, companion.Entitlements...)
	if svc, ok := companion.Services[serviceName]; ok && svc != nil {
		appCfg.Entitlements = append(appCfg.Entitlements, svc.Entitlements...)
		if svc.Frameworks != nil {
			appCfg.Frameworks = svc.Frameworks
		}
		if svc.Readiness != nil {
			appCfg.Readiness = svc.Readiness
		}
		if svc.Hooks != nil {
			appCfg.Hooks = svc.Hooks
		}
	}
	appCfg.Entitlements = deduplicateEntitlements(appCfg.Entitlements)
}

// composeAppLevelAgentHookDropped reports whether the app-level fallback config
// carries a top-level hooks.postStart.agent that a compose run will silently
// drop: compose apps have no app-level container, so appLevelLifecycleConfig's
// agent command is never sent to the agent. Callers warn on it so the drop is
// visible (Stage-A ValidateJSON only warns when a services map is present, and
// a compose companion has none). nil-safe at every level.
func composeAppLevelAgentHookDropped(appLevelCfg *appconfig.AppConfig) bool {
	return appLevelCfg != nil &&
		appLevelCfg.Hooks != nil &&
		appLevelCfg.Hooks.PostStart != nil &&
		appLevelCfg.Hooks.PostStart.Agent != ""
}

// buildComposeServiceConfigs synthesizes the per-service AppConfig for every
// compose service once: compose-derived fields, then x-wendy lifecycle config,
// then companion overrides. Returned warnings are printed once by the caller.
func buildComposeServiceConfigs(projectName string, cfg *composeConfig, companion *appconfig.AppConfig) (map[string]*appconfig.AppConfig, []string, error) {
	names := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	svcCfgs := make(map[string]*appconfig.AppConfig, len(cfg.Services))
	var warnings []string
	for _, name := range names {
		svc := cfg.Services[name]
		appCfg := composeAppConfig(projectName, name, svc, len(cfg.Services))

		readiness, hooks, xWendyWarnings, err := parseXWendy(svc.XWendy, name)
		if err != nil {
			return nil, nil, err
		}
		appCfg.Readiness, appCfg.Hooks = readiness, hooks
		warnings = append(warnings, xWendyWarnings...)

		applyComposeCompanion(appCfg, companion, name)

		svcCfgs[name] = appCfg
	}
	return svcCfgs, warnings, nil
}

// composeServiceLifecycleConfigs builds CLI-private lifecycle views from the
// fully merged Compose configs. Readiness/hooks may come from x-wendy or a
// companion service override, but HTTP entitlements come only from the
// matching companion services.<name> scope. Top-level companion HTTP remains
// inherited in each create config while executing once via appLevelCfg.
func composeServiceLifecycleConfigs(svcCfgs map[string]*appconfig.AppConfig, companion *appconfig.AppConfig) map[string]*appconfig.AppConfig {
	lifecycleCfgs := make(map[string]*appconfig.AppConfig, len(svcCfgs))
	for name, cfg := range svcCfgs {
		var serviceEntitlements []appconfig.Entitlement
		if companion != nil {
			if svc := companion.Services[name]; svc != nil {
				serviceEntitlements = svc.Entitlements
			}
		}
		lifecycleCfgs[name] = lifecycleConfig(cfg.AppID, cfg.ServiceName, cfg.Readiness, cfg.Hooks, serviceEntitlements)
	}
	return lifecycleCfgs
}

// deduplicateEntitlements returns a copy of ents with duplicates removed.
// Two entitlements are considered duplicates when their type and complete
// canonical annotation value are equal; the first occurrence is kept. This
// covers the common cases without collapsing parameterized same-type grants:
//   - GPU declared in both shared and per-service sections
//   - Network mode declared in both compose (network_mode:host) and companion
//   - Persist volumes declared multiple times with the same name and path
//
// Device, GPIO, allowlist, port, and every other entitlement parameter is part
// of EntitlementAnnotationValue, so (for example) ttyUSB0 and ttyUSB1 remain
// two serial entitlements.
func deduplicateEntitlements(ents []appconfig.Entitlement) []appconfig.Entitlement {
	seen := make(map[string]bool, len(ents))
	out := make([]appconfig.Entitlement, 0, len(ents))
	for _, e := range ents {
		key := e.Type + "\x00" + appconfig.EntitlementAnnotationValue(e)
		if !seen[key] {
			seen[key] = true
			out = append(out, e)
		}
	}
	return out
}

// serviceOrder returns service names sorted by depends_on so dependencies
// start before dependents. It returns an error if any depends_on entry
// references an undefined service. Cycles are ignored; remaining services are
// appended at the end.
func serviceOrder(cfg *composeConfig) ([]string, error) {
	// Build dependency map and validate that every dependency is a defined service.
	deps := make(map[string][]string, len(cfg.Services))
	for name, svc := range cfg.Services {
		var depends []string
		switch svc.DependsOn.Kind {
		case yaml.SequenceNode:
			_ = svc.DependsOn.Decode(&depends)
		case yaml.MappingNode:
			var m map[string]interface{}
			if svc.DependsOn.Decode(&m) == nil {
				for k := range m {
					depends = append(depends, k)
				}
			}
		}
		for _, dep := range depends {
			if _, ok := cfg.Services[dep]; !ok {
				return nil, fmt.Errorf("service %q depends on unknown service %q", name, dep)
			}
		}
		deps[name] = depends
	}

	var ordered []string
	visited := make(map[string]bool)

	var visit func(name string)
	visit = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		for _, dep := range deps[name] {
			visit(dep)
		}
		ordered = append(ordered, name)
	}

	for name := range cfg.Services {
		visit(name)
	}
	return ordered, nil
}

// serviceLogPalette is the fixed color rotation for service name prefixes.
var serviceLogPalette = []lipgloss.Color{
	tui.Sky500,                // cyan-ish
	tui.Amber500,              // yellow
	tui.Emerald400,            // green
	lipgloss.Color("#c084fc"), // magenta
	lipgloss.Color("#60a5fa"), // blue
	tui.Red500,                // red
}

// serviceLogWriter buffers output for a single service and writes complete
// lines prefixed with a color-coded, column-aligned service name.
// It is safe to call Write from a single goroutine; Flush drains any partial line.
type serviceLogWriter struct {
	mu     *sync.Mutex // shared with all writers so lines don't interleave
	dest   *os.File
	buf    strings.Builder
	prefix string // pre-rendered "[name]  " with padding and color
}

func newServiceLogWriters(names []string) (stdout, stderr map[string]*serviceLogWriter) {
	mu := &sync.Mutex{}
	maxLen := 0
	for _, n := range names {
		if len(n) > maxLen {
			maxLen = len(n)
		}
	}
	stdout = make(map[string]*serviceLogWriter, len(names))
	stderr = make(map[string]*serviceLogWriter, len(names))
	for i, name := range names {
		color := serviceLogPalette[i%len(serviceLogPalette)]
		style := lipgloss.NewStyle().Foreground(color).Bold(true)
		errStyle := lipgloss.NewStyle().Foreground(color).Bold(true)
		padding := strings.Repeat(" ", maxLen-len(name)+1)
		prefix := style.Render("["+name+"]") + padding
		stdout[name] = &serviceLogWriter{mu: mu, dest: os.Stdout, prefix: prefix}
		stderr[name] = &serviceLogWriter{mu: mu, dest: os.Stderr, prefix: errStyle.Render("["+name+"]") + padding}
	}
	return stdout, stderr
}

func (w *serviceLogWriter) Write(p []byte) {
	for _, b := range p {
		if b == '\n' {
			w.mu.Lock()
			fmt.Fprintln(w.dest, w.prefix+w.buf.String())
			w.buf.Reset()
			w.mu.Unlock()
		} else {
			w.buf.WriteByte(b)
		}
	}
}

func (w *serviceLogWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		fmt.Fprintln(w.dest, w.prefix+w.buf.String())
		w.buf.Reset()
	}
}

// runComposeWithAgent orchestrates a docker-compose project on a WendyOS device:
// builds and prepares service images, creates containers, and streams their
// combined output. When a companion wendy.json exists in the
// same directory it is merged to supply Wendy-specific config (entitlements,
// isolation, frameworks) without modifying the compose file.
func runComposeWithAgent(ctx context.Context, conn *grpcclient.AgentConnection, projectDir string, opts runOptions) error {
	cfg, composeFilename, err := parseComposeFile(projectDir)
	if err != nil {
		return err
	}
	if len(cfg.Services) == 0 {
		return fmt.Errorf("%s defines no services", composeFilename)
	}

	// Load an optional companion wendy.json from the same directory.
	companion, companionWarnings, err := appconfig.LoadComposeCompanion(projectDir)
	if err != nil {
		return fmt.Errorf("companion wendy.json: %w", err)
	}
	for _, w := range companionWarnings {
		cliLogln("warning: %s", w)
	}
	for _, w := range composeCompanionWarnings(companion, cfg) {
		cliLogln("warning: %s", w)
	}

	versionResp, err := agentVersionForRun(ctx, conn)
	if err != nil {
		return fmt.Errorf("querying device version: %w", err)
	}
	agentOS := versionResp.GetOs()
	architecture := versionResp.GetCpuArchitecture()
	if architecture == "" {
		architecture = "arm64"
	}
	platform := resolveAgentPlatform("", agentOS, architecture)
	if strings.EqualFold(agentOS, appconfig.PlatformDarwin) {
		if err := rejectUnsupportedMacRunProject("compose", platform); err != nil {
			return err
		}
	}

	if err := requireRegistryAuth(ctx, conn); err != nil {
		return err
	}

	regPort := registryPort(agentOS)

	if err := ensureAppleContainerSystemForBuilder(ctx, opts.builder, opts.yes); err != nil {
		return err
	}

	// Use the project directory name as the project name.
	projectName := strings.ToLower(filepath.Base(projectDir))
	// Synthesize every service's full create config (compose fields + x-wendy
	// lifecycle config + companion overrides), then derive separate CLI-private
	// lifecycle views below. Done BEFORE the image builds so an invalid x-wendy
	// config fails fast instead of costing a full multi-minute build.
	svcCfgs, xWendyWarnings, err := buildComposeServiceConfigs(projectName, cfg, companion)
	if err != nil {
		return err
	}
	for _, w := range xWendyWarnings {
		cliLogln("warning: %s", w)
	}
	if opts.isWatch() && !opts.detach {
		if err := opts.watchState.ensureLogStream(conn, composeWatchAppID(projectName, companion, svcCfgs)); err != nil {
			return err
		}
	}
	svcLifecycleCfgs := composeServiceLifecycleConfigs(svcCfgs, companion)
	portConfigs := []*appconfig.AppConfig{companion}
	for _, svc := range svcCfgs {
		portConfigs = append(portConfigs, svc)
	}
	if err := prepareVMAppPorts(ctx, conn, portConfigs...); err != nil {
		return err
	}

	// App-level lifecycle fallback: only a companion wendy.json can declare
	// top-level HTTP/readiness/hooks for a compose project (x-wendy is
	// per-service by construction). It fires once after ALL services have
	// started; the compose path has no service-subset flag (unlike --service on
	// the multi-service path), so that guarantee always holds here.
	var appLevelCfg *appconfig.AppConfig
	if companion != nil {
		appLevelCfg = appLevelLifecycleConfig(companion.AppID, companion)
	}
	// A compose app has no app-level container, so a companion's top-level
	// hooks.postStart.agent can never run — warn rather than drop it silently.
	// Stage-A ValidateJSON only warns about this when a services map is present,
	// which a compose companion (e.g. Examples/WendyMC/wendy.json) has none of.
	if composeAppLevelAgentHookDropped(appLevelCfg) {
		cliLogln("warning: top-level hooks.postStart.agent is ignored for compose apps (no app-level container exists); declare it under a service's x-wendy.hooks or the companion's services.<name>.hooks")
	}

	// Resolved once for the project: every service lands on this one device,
	// so a cuda: stage in any of them compiles against its GPU architecture.
	gpuArch := composeGPUArch(ctx, projectDir, cfg, conn)
	deviceKey := deviceFingerprintKey(versionResp)
	states := map[string]agentpb.AppRunningState{}
	if opts.watchState != nil || os.Getenv("WENDY_PUSH_SKIP") != "0" {
		states = deviceContainerStates(ctx, conn)
	}
	preserve := map[string]bool{}
	desiredHashes := map[string]string{}
	candidates := map[string]watchServiceCandidate{}

	// Prepare each build before starting the parallel scheduler. In particular,
	// compile Stagefiles to their generated Dockerfiles once, while errors can
	// still be attributed directly to the declaring service.
	jobs := make(map[string]composeBuildJob)
	for name, svc := range cfg.Services {
		ctxDir, dockerfile, buildArgs, err := composeBuildContext(svc, projectDir)
		if err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}

		allBuildArgs := map[string]string{
			"WENDY_PLATFORM": wendyPlatform(versionResp.GetDeviceType()),
			"WENDY_DEBUG":    fmt.Sprintf("%t", opts.debug),
		}
		// Mirror the single-container build path so compose-built Dockerfiles
		// see the same WendyOS device hints (e.g. for GPU base-image selection).
		applyDeviceBuildArgHints(allBuildArgs, versionResp)
		for k, v := range buildArgs {
			allBuildArgs[k] = v
		}

		appCfg := svcCfgs[name]
		imageIdentity := normalizeImageRef(svc.Image)
		contentPinned := imageRefContentPinned(svc.Image)
		if ctxDir != "" {
			// Compile a Stagefile (or apply safe in-memory Dockerfile fixes) the
			// same way the single-service path does — docker build itself knows
			// nothing about build.stagefile.yaml.
			sfOpts := append(debugStagefileOptions(opts.debug), frameworkStagefileOptions(appCfg.Frameworks)...)
			if dockerfile, err = prepareDockerBuildFile(ctxDir, dockerfile, gpuArch, sfOpts...); err != nil {
				return fmt.Errorf("service %s: %w", name, err)
			}
			imageIdentity, err = computeBuildInputHash(ctxDir, dockerfile, platform, allBuildArgs, composeEnv(svc))
			if err != nil {
				return fmt.Errorf("hashing service %s build inputs: %w", name, err)
			}
			// The prepared image name is part of the desired identity. A project
			// directory rename changes the Compose repository even when its source
			// bytes do not, so reusing the old preparation would be unsafe.
			imageIdentity = fmt.Sprintf("localhost:%d/%s-%s:latest@%s", regPort, projectName, name, imageIdentity)
			contentPinned, err = dockerfileBasesContentPinned(ctxDir, dockerfile)
			if err != nil {
				return fmt.Errorf("checking service %s base images: %w", name, err)
			}
		}

		restartPolicy := resolveRestartPolicy(opts)
		if restartPolicy.GetMode() == agentpb.RestartPolicyMode_DEFAULT && svc.Restart != "" {
			restartPolicy = composeRestartPolicy(svc.Restart)
		}
		cmd, extraArgs := composeArgv(svc)
		desiredHash, hashErr := composeServiceWatchHash(imageIdentity, appCfg, cmd, extraArgs, restartPolicy, composeEnv(svc))
		if hashErr == nil && contentPinned {
			desiredHashes[name] = desiredHash
			candidates[name] = watchServiceCandidate{appID: appCfg.AppID, containerName: appCfg.ContainerName(), desiredHash: desiredHash}
		}
		if ctxDir == "" {
			continue // uses pre-built image
		}

		jobs[name] = composeBuildJob{
			contextDir: ctxDir,
			dockerfile: dockerfile,
			repo:       fmt.Sprintf("%s-%s", projectName, name),
			buildArgs:  allBuildArgs,
		}
	}

	// Persistent fingerprints skip the expensive local OCI/chunk/device image
	// pipeline for ordinary runs. Watch preservation is stronger: it also leaves
	// an unchanged running container untouched, so merge those services into the
	// build skip set after evaluating the session-scoped watch state.
	skipBuild := planComposeBuildSkips(ctx, conn, deviceKey, desiredHashes, svcCfgs, states)
	if opts.watchState != nil {
		preserve = selectPreservedWatchServices(opts.watchState, deviceKey, candidates, states)
		for name := range preserve {
			skipBuild[name] = true
		}
	}
	for name := range skipBuild {
		delete(jobs, name)
	}
	if n := len(preserve); n > 0 {
		cliLogln("%d of %d Compose services unchanged and running; leaving them untouched.", n, len(cfg.Services))
	} else if n := len(skipBuild); n > 0 {
		cliLogln("%d of %d Compose services unchanged and already on device; skipping image preparation.", n, len(cfg.Services))
	}

	failedBuilds, preparedContent, err := buildComposeServicesParallel(ctx, conn, regPort, agentOS, opts.builder, platform, opts.chunking, jobs, opts.maxConcurrency, opts.quietBuild)
	if err != nil {
		return err
	}
	if len(failedBuilds) > 0 {
		return joinServiceErrors(failedBuilds)
	}
	for name, diffIDs := range preparedContent {
		if desiredHash := desiredHashes[name]; desiredHash != "" && len(diffIDs) > 0 {
			appCfg := svcCfgs[name]
			saveDeployFingerprint(serviceFingerprintKey(appCfg.AppID, name), deviceKey, deployFingerprint{
				InputHash:    desiredHash,
				AppVersion:   appCfg.Version,
				LayerDiffIDs: diffIDs,
			})
		}
	}

	cliRestartPolicy := resolveRestartPolicy(opts)

	// Create all containers in dependency order.
	ordered, err := serviceOrder(cfg)
	if err != nil {
		return err
	}
	if len(ordered) > 0 {
		adjustedPreserve := adjustSharedNamespacePreserve(ordered, preserve, svcCfgs[ordered[0]].Isolation)
		if len(adjustedPreserve) < len(preserve) {
			cliLogln("Shared-namespace primary changed; restarting the affected Compose service group.")
		}
		preserve = adjustedPreserve
	}
	preservedLifecycle := preservedServicesInOrder(ordered, preserve)
	if len(preserve) > 0 {
		ordered = filterPreservedServices(ordered, preserve)
		if len(ordered) == 0 {
			cliLogln("All Compose services are unchanged and running; nothing to redeploy.")
			if opts.detach || opts.deploy {
				return nil
			}
		}
	}

	for _, name := range ordered {
		svc := cfg.Services[name]
		appCfg := svcCfgs[name]

		// Determine image: built image or declared image. Public image refs
		// like "python:3.11-slim" must be canonicalised to "docker.io/library/…"
		// because the agent's containerd reference parser only accepts
		// fully-qualified names.
		ctxDir, _, _, _ := composeBuildContext(svc, projectDir)
		var imageName string
		if ctxDir != "" {
			imageName = fmt.Sprintf("localhost:%d/%s-%s:latest", regPort, projectName, name)
		} else if svc.Image != "" {
			imageName = normalizeImageRef(svc.Image)
		} else {
			return fmt.Errorf("service %s: no image or build directive", name)
		}

		appConfigData, err := json.Marshal(appCfg)
		if err != nil {
			return fmt.Errorf("marshaling config for service %s: %w", name, err)
		}

		// Split the compose command into argv: the first token becomes Cmd
		// (the agent runs strings.Fields on it, so it must contain no
		// whitespace) and the rest are passed verbatim through UserArgs so
		// arguments like a multi-line `python3 -c <script>` survive intact.
		cmd, extraArgs := composeArgv(svc)

		// CLI flags take precedence over per-service compose restart policies;
		// when the CLI didn't specify one (DEFAULT), honour the service's restart.
		restartPolicy := cliRestartPolicy
		if restartPolicy.GetMode() == agentpb.RestartPolicyMode_DEFAULT && svc.Restart != "" {
			restartPolicy = composeRestartPolicy(svc.Restart)
		}

		if warns := unsupportedComposeWarnings(svc); len(warns) > 0 {
			cliLogln("warning: service %q uses unsupported Compose fields (ignored by Wendy): %s",
				name, strings.Join(warns, ", "))
		}

		createReq := &agentpb.CreateContainerRequest{
			ImageName:     imageName,
			AppName:       appCfg.ContainerName(),
			AppConfig:     appConfigData,
			Cmd:           cmd,
			RestartPolicy: restartPolicy,
			UserArgs:      extraArgs,
			Env:           composeEnv(svc),
		}

		cliLogln("Creating container for service %s (%s)...", name, appCfg.ContainerName())
		if err := createContainerWithProgress(ctx, conn.ContainerService, createReq); err != nil {
			return fmt.Errorf("creating container for service %s: %w", name, err)
		}
		cliLogln("Container %s created.", appCfg.ContainerName())
	}

	if opts.deploy {
		cliLogln("All %d service containers created (not started).", len(cfg.Services))
		return nil
	}

	// Start all containers and stream their output concurrently.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	// The watch loop owns Ctrl-C for the whole session and leaves the project
	// running when it stops, so a watch cycle must not install its own handler.
	sigCh := make(chan os.Signal, 1)
	if !opts.isWatch() {
		signal.Notify(sigCh, os.Interrupt)
		defer signal.Stop(sigCh)
	}
	go func() {
		select {
		case <-sigCh:
		case <-runCtx.Done():
			return
		}
		// Stop in reverse dependency order.
		stopCtx := context.Background()
		stopped := 0
		fmt.Println()
		for i := len(ordered) - 1; i >= 0; i-- {
			name := ordered[i]
			appCfg := svcCfgs[name]
			cliLogln("Stopping %s...", name)
			// Use AppID (not ContainerName) so StopContainer's label-based lookup
			// finds the container regardless of whether a companion sets ServiceName.
			_, _ = conn.ContainerService.StopContainer(stopCtx, &agentpb.StopContainerRequest{
				AppName: appCfg.AppID,
			})
			stopped++
		}
		cliLogln("Stopped %d service(s).", stopped)
		runCancel()
	}()

	var runErr error
	if opts.detach {
		runErr = composeStartDetached(ctx, conn, ordered, svcCfgs, projectName)
	} else if opts.isWatch() {
		runErr = composeStartWatch(runCtx, conn, ordered, preservedLifecycle, svcCfgs, svcLifecycleCfgs, appLevelCfg, opts)
	} else {
		// Attached mode: stream output from all containers concurrently with
		// color-coded, column-aligned service name prefixes.
		stdoutWriters, stderrWriters := newServiceLogWriters(ordered)
		runErr = composeStartAndStream(runCtx, runCancel, conn, ordered, svcCfgs, svcLifecycleCfgs, appLevelCfg, stdoutWriters, stderrWriters, opts)
	}
	if runErr != nil {
		return runErr
	}
	if ctx.Err() == nil {
		for _, name := range ordered {
			if h := desiredHashes[name]; h != "" {
				appCfg := svcCfgs[name]
				opts.watchState.record(watchServiceKey(deviceKey, appCfg.AppID, name), h)
			}
		}
	}
	return nil
}

// composeStartDetached starts every compose service in dependency order,
// waiting for each Started ack, and returns with the containers left running.
// Each start carries the service's agent-side postStart hook as gRPC metadata,
// so in-container hooks still run on the device.
//
// No host-side lifecycle work: detached runs do not wait for readiness,
// announce the app URL, or fire host postStart hooks — see
// runPostStartIfReady's doc comment (WDY-2041).
func composeStartDetached(ctx context.Context, conn *grpcclient.AgentConnection, ordered []string, svcCfgs map[string]*appconfig.AppConfig, projectName string) error {
	for _, name := range ordered {
		appCfg := svcCfgs[name]
		stream, err := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(ctx, appCfg), &agentpb.StartContainerRequest{
			AppName: appCfg.ContainerName(),
		})
		if err != nil {
			return fmt.Errorf("starting service %s: %w", name, err)
		}
		if _, err := stream.Recv(); err != nil && err != io.EOF {
			return fmt.Errorf("waiting for service %s start: %w", name, err)
		}
		cliLogln("Service %s started.", name)
	}
	cliLogln("All services running in detached mode.")
	cliLogln("Run 'wendy device logs' to stream logs (filter a service with --app %s-<service>).", projectName)
	return nil
}

// composeStartWatch starts the changed service set and returns after every
// service has confirmed Started and first-successful-deploy lifecycle work has
// settled. Preserved services are not restarted, but any host lifecycle work
// left pending by an earlier canceled or failed attempt is retried. The session
// log stream independently includes services omitted from the start set.
func composeStartWatch(ctx context.Context, conn *grpcclient.AgentConnection, ordered, preservedLifecycle []string, svcCfgs, svcLifecycleCfgs map[string]*appconfig.AppConfig, appLevelCfg *appconfig.AppConfig, opts runOptions) error {
	runner := &serviceHookRunner{conn: conn, opts: opts}
	cycleCtx, cycleCancel := context.WithCancel(ctx)
	defer cycleCancel()
	var wg sync.WaitGroup
	errCh := make(chan error, len(ordered))

	for _, name := range ordered {
		appCfg := svcCfgs[name]
		wg.Add(1)
		go func(serviceName string, cfg, lifecycleCfg *appconfig.AppConfig) {
			defer wg.Done()
			startCtx, cancel := context.WithCancel(cycleCtx)
			defer cancel()
			stream, err := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(startCtx, cfg), &agentpb.StartContainerRequest{
				AppName: cfg.ContainerName(),
			})
			if err != nil {
				errCh <- fmt.Errorf("starting service %s: %w", serviceName, err)
				return
			}
			if err := awaitStarted(stream); err != nil {
				errCh <- fmt.Errorf("waiting for service %s start: %w", serviceName, err)
				return
			}
			cancel()
			runner.startAsync(cycleCtx, lifecycleCfg)
		}(name, appCfg, svcLifecycleCfgs[name])
	}
	wg.Wait()
	close(errCh)
	var startErrs []error
	for err := range errCh {
		startErrs = append(startErrs, err)
	}
	if err := errors.Join(startErrs...); err != nil {
		cycleCancel()
		runner.reap()
		return err
	}
	if cycleCtx.Err() != nil {
		cycleCancel()
		runner.reap()
		return cycleCtx.Err()
	}
	for _, name := range preservedLifecycle {
		runner.startAsync(cycleCtx, svcLifecycleCfgs[name])
	}
	runner.startAsync(cycleCtx, appLevelCfg)
	if len(ordered) > 0 {
		cliLogln("All services started.")
	}
	runner.reap()
	return cycleCtx.Err()
}

// composeStartAndStream starts all Compose services concurrently and multiplexes
// their output until every stream ends. It prefers AttachContainer and falls
// back to StartContainer when the agent does not support attachment. The caller
// owns Ctrl+C handling and cancels runCtx after stopping the services.
func composeStartAndStream(runCtx context.Context, runCancel context.CancelFunc, conn *grpcclient.AgentConnection, ordered []string, svcCfgs, svcLifecycleCfgs map[string]*appconfig.AppConfig, appLevelCfg *appconfig.AppConfig, stdoutWriters, stderrWriters map[string]*serviceLogWriter, opts runOptions) error {
	runner := &serviceHookRunner{conn: conn, opts: opts}
	var appName string
	if len(ordered) > 0 && svcCfgs[ordered[0]] != nil {
		appName = svcCfgs[ordered[0]].AppID
	}
	logSub := startRunLogSubscription(runCtx, conn, appName, os.Stdout, runLogStreamWarning)
	defer logSub.stop()

	var wg sync.WaitGroup
	errCh := make(chan error, len(ordered))

	// startedWg gates the app-level fallback on every service having reported
	// Started. Each service goroutine releases it exactly once — on its first
	// Started message, or via the deferred markStarted if its stream dies
	// first — so the fallback goroutine below can never leak.
	var startedWg sync.WaitGroup
	startedWg.Add(len(ordered))

	for _, name := range ordered {
		appCfg := svcCfgs[name]

		wg.Add(1)
		go func(serviceName, containerID string, svcCfg, lifecycleCfg *appconfig.AppConfig) {
			defer wg.Done()
			markStarted := sync.OnceFunc(startedWg.Done)
			defer markStarted()
			outW := stdoutWriters[serviceName]
			errW := stderrWriters[serviceName]
			defer outW.Flush()
			defer errW.Flush()

			// openStart falls back to the server-streaming StartContainer RPC,
			// used when the agent is too old to support AttachContainer. The
			// agent reads the postStart-hook metadata on both RPCs, so the
			// fallback must carry it too.
			openStart := func() (containerOutputStream, error) {
				startStream, startErr := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(runCtx, svcCfg), &agentpb.StartContainerRequest{
					AppName: containerID,
				})
				if startErr != nil {
					return nil, fmt.Errorf("starting service %s: %w", serviceName, startErr)
				}
				return startStream, nil
			}

			var outStream containerOutputStream
			attached := false
			attachStream, streamErr := conn.ContainerService.AttachContainer(contextWithPostStartAgentHook(runCtx, svcCfg))
			if streamErr == nil {
				streamErr = attachStream.Send(&agentpb.AttachContainerRequest{
					RequestType: &agentpb.AttachContainerRequest_AppName{AppName: containerID},
				})
				// Compose never forwards stdin; half-close the send side so the
				// container sees stdin EOF instead of hanging on a read.
				_ = attachStream.CloseSend()
			}
			if streamErr != nil {
				s, err := openStart()
				if err != nil {
					errCh <- err
					return
				}
				outStream = s
			} else {
				outStream = attachStream
				attached = true
			}

			// hookFired guards against a double fire: the Unimplemented→
			// StartContainer fallback below REPLAYS the Started message, so a
			// stream swap must not re-run this service's lifecycle sequence.
			hookFired := false
			gotFirstResponse := false
			for {
				resp, recvErr := outStream.Recv()
				if recvErr == io.EOF {
					return
				}
				if recvErr != nil {
					if runCtx.Err() != nil {
						return
					}
					// Older agents reject AttachContainer with Unimplemented on
					// the first Recv rather than at open/send time; fall back
					// silently to StartContainer.
					if attached && !gotFirstResponse && status.Code(recvErr) == codes.Unimplemented {
						s, err := openStart()
						if err != nil {
							errCh <- err
							return
						}
						outStream = s
						attached = false
						continue
					}
					errCh <- fmt.Errorf("service %s: %w", serviceName, recvErr)
					return
				}
				gotFirstResponse = true
				if resp.GetStarted() != nil && !hookFired {
					// This service's task is running: fire its readiness→
					// announce→postStart sequence on a runner goroutine so a
					// slow or failing probe never stalls this log loop.
					hookFired = true
					markStarted()
					runner.startAsync(runCtx, lifecycleCfg)
				}
				if out := resp.GetStdoutOutput(); out != nil {
					outW.Write(out.GetData())
				}
				if out := resp.GetStderrOutput(); out != nil {
					errW.Write(out.GetData())
				}
			}
		}(name, appCfg.ContainerName(), appCfg, svcLifecycleCfgs[name])
	}

	// App-level fallback: fires once after every service has reported Started
	// (or its stream has died). Registered on the runner (reaped after the
	// stream teardown), NOT on wg — so wg.Wait() below returns as soon as the
	// streams end, and the teardown's runCancel() then cancels an in-flight
	// readiness wait so the hook is SUPPRESSED at natural stream end rather
	// than fired onto a stack that has already exited (mirrors multibuild).
	// No deadlock: by the time reap() runs, every stream goroutine has exited
	// and released startedWg via its deferred markStarted, and runCtx is
	// canceled, so this goroutine's startedWg.Wait() and any readiness wait
	// both return promptly.
	runner.spawn(func() {
		startedWg.Wait()
		if runCtx.Err() == nil {
			runner.runOne(runCtx, runCtx, appLevelCfg)
		}
	})

	cliLogln("All services started.")

	wg.Wait()

	// The run has ended (streams closed, or Ctrl+C already canceled runCtx).
	// Capture the interrupt state first, then cancel to abort in-flight
	// readiness waits and kill tracked cli-hook children, and reap so no hook
	// goroutine or child outlives this call — mirroring run.go's
	// `runCancel(); postStartCmd.Wait()`.
	interrupted := runCtx.Err() != nil
	runCancel()
	runner.reap()

	select {
	case err := <-errCh:
		if !interrupted {
			return err
		}
	default:
	}

	if !interrupted {
		cliLogln("\nAll services stopped.")
	}
	return nil
}
