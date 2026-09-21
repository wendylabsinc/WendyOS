package commands

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// deployFingerprint records what was last successfully deployed for a given app
// or service to a given device from this machine. It lets run paths skip the
// whole build → OCI export → chunk-diff → reassemble pipeline when nothing that
// could affect the image or effective service configuration has changed. The
// single-container path also uses it to ensure the existing container
// is running without recreating it.
//
// This is intentionally a local-only, best-effort optimization (WDY fast path):
// the fingerprint is trusted as-is, and the device is only consulted to confirm
// the app still exists and learn whether it is running. Any mismatch, missing
// fingerprint, or RPC hiccup falls back to the normal deploy, so a stale cache
// can never deploy the wrong code — at worst it triggers an unnecessary build.
type deployFingerprint struct {
	// InputHash is computed by computeBuildInputHash over everything that can
	// affect a single built image, or by a service desired-state hash that also
	// includes its create-time configuration.
	InputHash string `json:"inputHash"`
	// AppVersion is the wendy.json version at deploy time, used as a cheap
	// cross-check against the version the device reports.
	AppVersion string `json:"appVersion,omitempty"`
	// LayerDiffIDs are the uncompressed layer diff IDs of the image content that
	// was actually pushed to this device on the last successful deploy. A skip is
	// only permitted when the device still holds every one of these layers (see
	// deviceHasAllLayers): an unchanged input hash alone does not prove the device
	// still has the content — blobs can be GC'd, a partial push can leave the
	// manifest without its blobs, or the local base image can be rebuilt without
	// changing the hash. Verifying the layers are present closes the WDY-1824 hole
	// where the CLI skipped the push and reported success while the device kept
	// running a stale/partial image. Empty (e.g. recorded by a path that doesn't
	// surface diff IDs) means "cannot verify" → never skip.
	LayerDiffIDs []string `json:"layerDiffIds,omitempty"`
}

// deployRuntimeIdentity is the subset of a single-container deployment that
// affects the container the agent creates. Build-only and CLI-only settings
// are deliberately absent: changing a readiness probe or a host-side hook must
// not invalidate an otherwise reusable container, while changing resources,
// entitlements, isolation, framework wiring, arguments, environment, or the
// restart policy must.
//
// Keep AppConfig as a value so newly-added device runtime fields fail safe: they
// are included unless runtimeAppConfig explicitly clears them below.
type deployRuntimeIdentity struct {
	AppConfig     appconfig.AppConfig    `json:"appConfig"`
	UserArgs      []string               `json:"userArgs,omitempty"`
	Env           []string               `json:"env,omitempty"`
	RestartPolicy *agentpb.RestartPolicy `json:"restartPolicy,omitempty"`
}

// computeDeployDesiredHash combines the already-computed build-input hash
// (Dockerfile/context, target platform, build args, and deploy environment)
// with the effective create/start configuration. The old fast path used the
// build hash directly, which meant a resource, entitlement, isolation,
// user-arg, or restart-policy-only change could incorrectly leave the old
// container running.
func computeDeployDesiredHash(buildInputHash string, appCfg *appconfig.AppConfig, userArgs, env []string, restartPolicy *agentpb.RestartPolicy) (string, error) {
	if buildInputHash == "" || appCfg == nil {
		return "", fmt.Errorf("build input hash and app config are required")
	}

	runtimeCfg := *appCfg
	// These fields influence local build selection, native file deployment, or
	// CLI lifecycle behavior, but are not consumed when the Linux container is
	// created. Excluding them lets readiness/hook-only edits retain the running
	// container without weakening device-runtime correctness.
	runtimeCfg.Platform = ""
	runtimeCfg.Xcode = nil
	runtimeCfg.Run = nil // effective arguments are carried separately below
	runtimeCfg.Readiness = nil
	hook := postStartAgentHook(appCfg)
	if hook != "" {
		runtimeCfg.Hooks = &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{Agent: hook},
		}
	} else {
		runtimeCfg.Hooks = nil
	}
	runtimeCfg.Python = nil
	runtimeCfg.Files = nil
	runtimeCfg.Brewfile = ""
	runtimeCfg.Env = nil // resolved values are carried separately below
	runtimeCfg.Services = nil

	payload, err := json.Marshal(deployRuntimeIdentity{
		AppConfig:     runtimeCfg,
		UserArgs:      userArgs,
		Env:           env,
		RestartPolicy: restartPolicy,
	})
	if err != nil {
		return "", fmt.Errorf("marshaling deploy runtime identity: %w", err)
	}

	h := sha256.New()
	io.WriteString(h, "wendy-deploy-desired-state-v1\n")
	io.WriteString(h, buildInputHash)
	h.Write([]byte{'\n'})
	h.Write(payload)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// deployFingerprintPath returns the on-disk location for an app+device
// fingerprint. deviceKey identifies the target device (see deviceFingerprintKey).
func deployFingerprintPath(appID, deviceKey string) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cacheDir, "wendy", "deploy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := sanitizeCacheKey(appID) + "_" + sanitizeCacheKey(deviceKey) + ".json"
	return filepath.Join(dir, name), nil
}

// deviceFingerprintKey derives a stable per-device key from the agent version
// response. The agent's public key is the most stable identifier when present;
// otherwise we fall back to a generic key (the worst case is that two devices
// without public keys share a fingerprint slot and occasionally rebuild).
func deviceFingerprintKey(v *agentpb.GetAgentVersionResponse) string {
	if pk := v.GetPublicKey(); pk != "" {
		sum := sha256.Sum256([]byte(pk))
		return hex.EncodeToString(sum[:8])
	}
	return "default"
}

// sanitizeCacheKey makes a string safe to use as a filename component.
func sanitizeCacheKey(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

func loadDeployFingerprint(appID, deviceKey string) (*deployFingerprint, bool) {
	p, err := deployFingerprintPath(appID, deviceKey)
	if err != nil {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	var fp deployFingerprint
	if err := json.Unmarshal(data, &fp); err != nil {
		return nil, false
	}
	if fp.InputHash == "" {
		return nil, false
	}
	return &fp, true
}

func saveDeployFingerprint(appID, deviceKey string, fp deployFingerprint) {
	p, err := deployFingerprintPath(appID, deviceKey)
	if err != nil {
		return
	}
	data, err := json.Marshal(fp)
	if err != nil {
		return
	}
	// Best-effort: a failed write just means the next run rebuilds.
	_ = os.WriteFile(p, data, 0o644)
}

// computeBuildInputHash hashes everything that can change the built image:
// the resolved Dockerfile contents, the build context files (honoring
// .dockerignore conservatively), and the sorted build args + platform. deployEnv
// is hashed too — it is applied at container create rather than at build, so a
// changed env must still force a redeploy even when the image is identical.
//
// Correctness rule: the hash MUST change whenever anything buildkit could use
// changes. We therefore over-approximate — we hash the whole context minus
// .dockerignore'd paths (a superset of what COPY/ADD can pull in), and any
// .dockerignore pattern we cannot confidently parse is simply not applied (so a
// file is hashed rather than skipped). This can only cause an unnecessary
// rebuild, never a missed change.
func computeBuildInputHash(cwd, dockerfile, platform, backend string, buildArgs map[string]string, deployEnv []string) (string, error) {
	h := sha256.New()
	// Salt for the deploy fingerprint. Changing this string invalidates every
	// recorded fingerprint, forcing one honest rebuild per app — do that
	// whenever the hash inputs below change (as they did when the effective
	// Stagefile backend was added).
	io.WriteString(h, "wendy-deploy-fingerprint\n")
	io.WriteString(h, "platform="+platform+"\n")
	io.WriteString(h, "backend="+backend+"\n")

	// deployEnv arrives sorted from resolveServiceEnv; --env order is the
	// user's and is hashed as given.
	for _, kv := range deployEnv {
		io.WriteString(h, "env "+kv+"\n")
	}

	keys := make([]string, 0, len(buildArgs))
	for k := range buildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		io.WriteString(h, "arg "+k+"="+buildArgs[k]+"\n")
	}

	// Resolve and hash the Dockerfile.
	dfPath := filepath.Join(cwd, "Dockerfile")
	if dockerfile != "" {
		resolved, err := confinedDockerfilePath(cwd, dockerfile)
		if err != nil {
			return "", err
		}
		dfPath = resolved
	}
	dfData, err := os.ReadFile(dfPath)
	if err != nil {
		return "", fmt.Errorf("reading Dockerfile for fingerprint: %w", err)
	}
	io.WriteString(h, fmt.Sprintf("dockerfile %d\n", len(dfData)))
	h.Write(dfData)

	// Hash the build context, honoring the ignore file the build itself uses:
	// BuildKit gives <dockerfile>.dockerignore precedence over .dockerignore
	// (the Stagefile flow derives a deny-all allowlist there), so the walk must
	// follow the same file or it hashes paths the build can never see.
	ignore := loadDockerIgnoreForBuild(cwd, dfPath)
	var files []string
	err = filepath.WalkDir(cwd, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(cwd, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if ignore.matches(rel + "/") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if ignore.matches(rel) {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walking build context for fingerprint: %w", err)
	}
	sort.Strings(files)
	for _, rel := range files {
		f, err := os.Open(filepath.Join(cwd, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		io.WriteString(h, "file "+rel+"\n")
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
	}

	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// dockerfileBasesContentPinned reports whether rebuilding this Dockerfile is
// independent of mutable registry tags. Persistent no-build skipping is only
// safe when every external base is scratch or digest-pinned; merely proving
// that the previously-built layers still exist on the device does not prove a
// mutable FROM tag still resolves to those layers.
//
// This intentionally accepts only a small, unambiguous subset of Dockerfile
// syntax. ARG-expanded bases and malformed/uncertain instructions fail closed
// to a normal build. Multi-stage references to an earlier named stage are safe.
func dockerfileBasesContentPinned(cwd, dockerfile string) (bool, error) {
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	dfPath, err := confinedDockerfilePath(cwd, dockerfile)
	if err != nil {
		return false, err
	}
	f, err := os.Open(dfPath)
	if err != nil {
		return false, fmt.Errorf("reading Dockerfile base images: %w", err)
	}
	defer f.Close()

	var logical strings.Builder
	stages := make(map[string]struct{})
	foundFrom := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		continued := strings.HasSuffix(line, "\\")
		line = strings.TrimSpace(strings.TrimSuffix(line, "\\"))
		if logical.Len() > 0 {
			logical.WriteByte(' ')
		}
		logical.WriteString(line)
		if continued {
			continue
		}

		fields := strings.Fields(logical.String())
		logical.Reset()
		if len(fields) == 0 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		foundFrom = true
		i := 1
		for i < len(fields) && strings.HasPrefix(fields[i], "--") {
			// FROM flags use --key=value. Reject split/unknown forms instead of
			// guessing which following token is the image reference.
			if !strings.Contains(fields[i], "=") {
				return false, nil
			}
			i++
		}
		if i >= len(fields) {
			return false, nil
		}
		base := fields[i]
		_, priorStage := stages[strings.ToLower(base)]
		if !strings.EqualFold(base, "scratch") && !priorStage {
			parsed, parseErr := reference.ParseAnyReference(base)
			if parseErr != nil {
				return false, nil
			}
			if _, pinned := parsed.(reference.Digested); !pinned {
				return false, nil
			}
		}

		if i+2 < len(fields) && strings.EqualFold(fields[i+1], "AS") {
			alias := strings.ToLower(fields[i+2])
			if alias == "" || strings.HasPrefix(alias, "$") {
				return false, nil
			}
			stages[alias] = struct{}{}
		} else if i+1 != len(fields) {
			return false, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("scanning Dockerfile base images: %w", err)
	}
	if logical.Len() != 0 {
		return false, nil
	}
	return foundFrom, nil
}

// dockerIgnore is a conservative .dockerignore matcher. It only excludes a path
// when a pattern confidently matches; negation (!) and patterns it cannot parse
// are ignored, which keeps the matcher safe (it can only under-exclude, leading
// to extra files in the hash and at worst an unnecessary rebuild).
type dockerIgnore struct {
	patterns  []string
	negations []string // re-include patterns (lines starting with '!')
}

func loadDockerIgnore(cwd string) *dockerIgnore {
	return loadDockerIgnoreFile(filepath.Join(cwd, ".dockerignore"))
}

// loadDockerIgnoreForBuild picks the ignore file BuildKit would use for this
// dockerfile: <dockerfile>.dockerignore when present, else the context's
// .dockerignore.
func loadDockerIgnoreForBuild(cwd, dockerfilePath string) *dockerIgnore {
	if dockerfilePath != "" {
		perDockerfile := dockerfilePath + ".dockerignore"
		if fi, err := os.Stat(perDockerfile); err == nil && fi.Mode().IsRegular() {
			return loadDockerIgnoreFile(perDockerfile)
		}
	}
	return loadDockerIgnore(cwd)
}

func loadDockerIgnoreFile(path string) *dockerIgnore {
	di := &dockerIgnore{}
	f, err := os.Open(path)
	if err != nil {
		return di
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := strings.HasPrefix(line, "!")
		line = strings.TrimPrefix(line, "!")
		line = strings.TrimPrefix(line, "/")
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			continue
		}
		if negate {
			di.negations = append(di.negations, line)
		} else {
			di.patterns = append(di.patterns, line)
		}
	}
	// A scan error just yields a partial pattern set; the matcher stays safe
	// (fewer exclusions → more files hashed → at worst an unnecessary rebuild).
	_ = sc.Err()
	return di
}

// matches reports whether rel (a forward-slash relative path; directories carry
// a trailing slash) is excluded. A path matching any negation pattern is always
// kept (hashed): negations re-include files, so honoring them as "force include"
// keeps the matcher safe — it can only over-hash, never under-detect a change.
func (di *dockerIgnore) matches(rel string) bool {
	clean := strings.TrimSuffix(rel, "/")
	isDir := strings.HasSuffix(rel, "/")
	for _, neg := range di.negations {
		if matchIgnorePattern(neg, clean) {
			return false
		}
		// A directory with a re-included descendant must stay walkable: with
		// "*" + "!src/app.py", skipping src/ wholesale would hide the allowlisted
		// file's changes (the unsafe direction — a missed change, not an extra
		// rebuild).
		if isDir && strings.HasPrefix(neg, clean+"/") {
			return false
		}
	}
	for _, pat := range di.patterns {
		if matchIgnorePattern(pat, clean) {
			return true
		}
	}
	return false
}

// matchIgnorePattern reports whether a single (already sign/slash-trimmed)
// pattern matches the cleaned relative path.
func matchIgnorePattern(pat, clean string) bool {
	// Exact path or directory-prefix match.
	if clean == pat || strings.HasPrefix(clean, pat+"/") {
		return true
	}
	// Glob against the full relative path.
	if ok, err := path.Match(pat, clean); err == nil && ok {
		return true
	}
	// Glob against the basename for patterns without a separator (e.g. *.pyc).
	if !strings.Contains(pat, "/") {
		if ok, err := path.Match(pat, path.Base(clean)); err == nil && ok {
			return true
		}
	}
	return false
}

// tryDeployFastPath returns false when the deployment cannot be verified and
// the normal build path should run. Once verified, it returns true and handles
// the requested lifecycle, including foreground logs. Lifecycle errors are
// returned with true so they cannot trigger a redundant build and redeploy.
func tryDeployFastPath(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, deviceKey, inputHash string, opts runOptions) (bool, error) {
	fp, ok := loadDeployFingerprint(appCfg.AppID, deviceKey)
	if !ok || fp.InputHash != inputHash {
		return false, nil
	}

	// Unchanged inputs are necessary but not sufficient: the device must still
	// hold the image content we pushed last time. Without this check a skip can
	// leave the device running a stale/partial image while the CLI reports
	// success (WDY-1824) — e.g. blobs GC'd, a half-completed push, or a rebuilt
	// local base image that never changed the input hash.
	if !deviceHasAllLayers(ctx, conn, fp.LayerDiffIDs) {
		return false, nil
	}

	state, found, err := lookupAppState(ctx, conn, appCfg.AppID)
	if err != nil || !found {
		// Device unreachable for the query or app no longer present — rebuild.
		return false, nil
	}

	if !opts.detach {
		cliLogln("No changes detected; reusing existing %s.", containerDisplayName(appCfg))
		if state == agentpb.AppRunningState_RUNNING {
			if opts.isWatch() {
				cmd := runPostStartIfReady(ctx, opts.watchState.hookContext(ctx), conn, appCfg, opts)
				opts.watchState.reapCommand(cmd)
				return true, nil
			}
			return true, followExistingContainer(ctx, conn, appCfg, opts)
		}
		return true, startExistingContainer(ctx, conn, appCfg, opts)
	}

	if state == agentpb.AppRunningState_RUNNING {
		cliLogln("No changes detected; %s is already up to date and running.", containerDisplayName(appCfg))
		// Detached deploys don't run host-side postStart or block on readiness; see
		// runPostStartIfReady's doc comment. The container is untouched, so the
		// agent-side hook cannot re-run either.
		return true, nil
	}

	// Present but stopped — start it without rebuilding. Mirror the normal
	// detached deploy path so the fast path stays a transparent optimization:
	// attach the agent-side postStart hook to the start RPC (via context
	// metadata). Detached deploys do not fire the host-side postStart hook.
	if _, err := conn.ContainerService.StartContainer(contextWithPostStartAgentHook(ctx, appCfg), &agentpb.StartContainerRequest{
		AppName:       appCfg.AppID,
		RestartPolicy: resolveRestartPolicy(opts),
	}); err != nil {
		// Could not start the existing container; fall back to a full deploy.
		return false, nil
	}
	cliLogln("No changes detected; started existing %s.", containerDisplayName(appCfg))
	return true, nil
}

// containerExitDetail returns a short human summary of why appID's container
// stopped (e.g. "container crashed (exit 1)"), or "" if it's running, the cause
// wasn't recorded, or the lookup fails. Best-effort — it exists only to enrich
// a readiness failure, so it must never itself error out the deploy.
func containerExitDetail(ctx context.Context, conn *grpcclient.AgentConnection, appID string) string {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stream, err := conn.ContainerService.ListContainers(cctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return ""
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			break
		}
		c := resp.GetContainer()
		if c == nil || !strings.EqualFold(c.GetAppName(), appID) {
			continue
		}
		// Anything that isn't running counts — a crash-looping app (WDY-1826)
		// reports CRASH_LOOPING, not STOPPED, and its exit detail is exactly
		// what a readiness failure needs to explain itself.
		if c.GetRunningState() != agentpb.AppRunningState_RUNNING {
			if s := terminationSummary(c.GetTerminationReason(), c.GetExitCode()); s != "" {
				return "container " + s
			}
		}
		break
	}
	return ""
}

// warnReadiness logs a readiness failure, appending why the container stopped
// when the agent recorded a reason — so a bare "timed out" isn't the only thing
// the user sees when a startup crash is the real cause (WDY-1819).
func warnReadiness(ctx context.Context, conn *grpcclient.AgentConnection, appID string, err error) {
	msg := err.Error()
	if d := containerExitDetail(ctx, conn, appID); d != "" {
		msg += " — " + d
	}
	cliLogln("Warning: %s", msg)
}

// deviceHasAllLayers reports whether the device's content store still holds
// every one of diffIDs — i.e. the actual image blobs, not just a container
// record or a registry tag. It is the content check that gates every push-skip
// (WDY-1824): the local fingerprint only proves the inputs are unchanged since
// we last pushed, never that the device still has what we pushed.
//
// It is deliberately fail-closed: an empty diffID list (we never recorded what
// was deployed, so we cannot verify it), an agent too old for QueryLayers
// (Unimplemented), any RPC error, or a single missing layer all return false so
// the caller falls back to a real build+push. Only a device that confirms every
// layer is present authorizes a skip.
func deviceHasAllLayers(ctx context.Context, conn *grpcclient.AgentConnection, diffIDs []string) bool {
	if len(diffIDs) == 0 {
		return false
	}
	present, ok := devicePresentLayers(ctx, conn, diffIDs)
	if !ok {
		return false
	}
	for _, id := range diffIDs {
		if !present[id] {
			return false
		}
	}
	return true
}

// devicePresentLayers performs one fail-closed layer-presence query and returns
// the confirmed set. Keeping the raw set available lets multi-service planners
// batch every candidate service into one RPC instead of paying one round trip
// per service. The bool is false for old agents and all other RPC failures.
func devicePresentLayers(ctx context.Context, conn *grpcclient.AgentConnection, diffIDs []string) (map[string]bool, bool) {
	if len(diffIDs) == 0 {
		return nil, false
	}
	resp, err := conn.ContainerService.QueryLayers(ctx, &agentpb.QueryLayersRequest{DiffIds: diffIDs})
	if err != nil {
		return nil, false
	}
	present := make(map[string]bool, len(resp.GetPresent()))
	for _, p := range resp.GetPresent() {
		present[p.GetDiffId()] = true
	}
	return present, true
}

// lookupAppState queries the device for the running state of a single app.
func lookupAppState(ctx context.Context, conn *grpcclient.AgentConnection, appID string) (agentpb.AppRunningState, bool, error) {
	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return agentpb.AppRunningState_STOPPED, false, err
	}
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return agentpb.AppRunningState_STOPPED, false, err
		}
		c := resp.GetContainer()
		if c == nil {
			continue
		}
		if strings.EqualFold(c.GetAppName(), appID) {
			return c.GetRunningState(), true, nil
		}
	}
	return agentpb.AppRunningState_STOPPED, false, nil
}
