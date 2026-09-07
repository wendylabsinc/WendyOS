package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/espidftoolchain"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/optimize"
	"github.com/wendylabsinc/wendy/go/internal/cli/swifttoolchain"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/buildargs"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/stagefile"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// stagefileSourceName is the canonical Stagefile source filename. A project may
// also carry variants ("prod.stagefile.yaml"); stagefile.SourceNames enumerates
// the whole family and this one is the member every picker prefers.
const stagefileSourceName = stagefile.SourceName

// generatedDockerfileName is the internal build artifact prepareDockerBuildFile
// writes for the canonical Stagefile (or for an auto-fixed Dockerfile copy).
// The writer, the build-file-detection exclusion, and the watch ignore list all
// key off this one name so they can't drift.
const generatedDockerfileName = "Dockerfile.generated"

// generatedDockerignoreName is the ignore file BuildKit pairs with
// generatedDockerfileName (per-Dockerfile <name>.dockerignore precedence),
// letting the Stagefile compiler's derived allowlist apply without touching a
// user-authored .dockerignore.
const generatedDockerignoreName = generatedDockerfileName + ".dockerignore"

// generatedBuildFileFor returns the compiled-Dockerfile filename for a Stagefile
// source, and the .dockerignore BuildKit pairs with it.
//
// The canonical source keeps the historical bare "Dockerfile.generated" — users
// have it in .gitignore and CI globs — while each variant gets its own
// "Dockerfile.generated.<variant>". Distinct names are not cosmetic: compose
// routinely points several services at ONE build context with different build
// files, so two variants compiling into a shared context would otherwise race
// to overwrite a single artifact and could build each other's Dockerfile.
func generatedBuildFileFor(source string) (dockerfileName, dockerignoreName string) {
	variant, ok := stagefile.SourceVariant(source)
	if !ok || stagefile.IsCanonicalSourceName(source) {
		return generatedDockerfileName, generatedDockerignoreName
	}
	name := generatedDockerfileName + "." + variant
	return name, name + ".dockerignore"
}

// isGeneratedBuildFileName reports whether name is one of the build artifacts
// the CLI itself writes into a project dir — "Dockerfile.generated" and every
// "Dockerfile.generated.<variant>".
//
// A prefix test rather than an equality test because the variant names are
// themselves valid container-build-file names ("Dockerfile.generated.prod"
// matches validDockerfileNameRe), so without this each compiled variant would
// reappear in the next run's picker as a rival user build file.
func isGeneratedBuildFileName(name string) bool {
	return name == generatedDockerfileName || strings.HasPrefix(name, generatedDockerfileName+".")
}

// isStagefileLockName reports whether name is the lockfile of some Stagefile in
// the family — a build artifact the compiler maintains, not a project source.
func isStagefileLockName(name string) bool {
	return stagefile.IsLockName(name)
}

// stagefileSourceForGenerated inverts generatedBuildFileFor: given a compiled
// build file it names the Stagefile that would have produced it. ok is false for
// anything outside the generated namespace.
//
// It answers from the filename alone, so "Dockerfile.generated" maps back to the
// canonical Stagefile even when this particular copy came from the auto-fix path
// instead. Callers (nativeBuildEligibility) then fail to read that source and
// bail, which is the same answer they gave before variants existed.
func stagefileSourceForGenerated(name string) (string, bool) {
	base := filepath.Base(name)
	if base == generatedDockerfileName {
		return stagefileSourceName, true
	}
	variant, found := strings.CutPrefix(base, generatedDockerfileName+".")
	if !found {
		return "", false
	}
	source := variant + ".stagefile.yaml"
	if !stagefile.IsSourceName(source) {
		return "", false
	}
	return source, true
}

// writeGeneratedFile writes data to path only when the current content
// differs, via a same-directory temp file + rename. The rename keeps
// concurrent readers (parallel service builds sharing a context dir, buildx
// reading .dockerignore at context-send time) from ever observing a
// truncated file, and skipping the no-op write matters for `wendy watch`:
// these files live inside the watched root, and rewriting identical bytes on
// every build would re-trigger the watcher mid-deploy.
func writeGeneratedFile(path string, data []byte) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// neighborExecCommandContext is an overridable wrapper around exec.CommandContext
// used by neighbor-table helpers. Tests can replace this variable to stub
// command execution and outputs.
var neighborExecCommandContext = exec.CommandContext

var (
	imageBuilderCommandContext = exec.CommandContext
	imageBuilderLookPath       = exec.LookPath
	imageBuilderHostGOOS       = func() string { return runtime.GOOS }
	imageBuilderHostGOARCH     = func() string { return runtime.GOARCH }
)

const (
	imageBuilderDocker         = "docker"
	imageBuilderAppleContainer = "apple-container"
	imageBuilderBuildkit       = "buildkit"
)

var buildDockerProjectWithDocker = buildDockerProject

func normalizeImageBuilder(builder string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(builder)) {
	case "":
		return imageBuilderDocker, nil
	case imageBuilderDocker:
		return imageBuilderDocker, nil
	case imageBuilderAppleContainer:
		return imageBuilderAppleContainer, nil
	case imageBuilderBuildkit:
		return imageBuilderBuildkit, nil
	default:
		return "", fmt.Errorf("invalid value %q for --builder: must be one of docker, apple-container, or buildkit", builder)
	}
}

func imageBuilderWasExplicit(builder string) bool {
	return strings.TrimSpace(builder) != ""
}

func shouldAutoAttemptAppleContainerBuilder() bool {
	return imageBuilderHostGOOS() == "darwin" && imageBuilderHostGOARCH() == "arm64"
}

// shouldUseBuildkitOnDevice reports whether an unspecified builder should default
// to BuildKit: true only when running inside the device (WENDY_AGENT_SOCKET set)
// and Docker is unavailable. Off-device, or when docker exists, behavior is
// unchanged.
func shouldUseBuildkitOnDevice() bool {
	if os.Getenv("WENDY_AGENT_SOCKET") == "" {
		return false
	}
	if _, err := imageBuilderLookPath("docker"); err == nil {
		return false
	}
	return true
}

func logAppleContainerFallback(w io.Writer, _ error) {
	fmt.Fprintln(w, "[WARN] Apple Container unavailable or failed; falling back to Docker. Use --builder apple-container to require Apple Container.")
}

func safeCommandOutputSummary(out []byte, max int) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// redactBuildArgsForLog returns a copy of a builder command's args with every
// --build-arg value masked (the key is kept for debugging). Build-arg values
// can carry secrets, and these command lines are written to stderr and, under
// --quiet/`wendy watch`, buffered to disk — so they must never contain raw
// values.
func redactBuildArgsForLog(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "--build-arg" {
			if k, _, ok := strings.Cut(out[i+1], "="); ok && k != "" {
				out[i+1] = k + "=<redacted>"
			}
		}
	}
	return out
}

func registryImageUsesLoopbackRegistry(image string) bool {
	registry, _, ok := strings.Cut(image, "/")
	if !ok || registry == "" {
		return false
	}
	return registryAddrUsesLoopback(registry)
}

func registryAddrUsesLoopback(registry string) bool {
	host := registry
	if splitHost, _, err := net.SplitHostPort(registry); err == nil {
		host = splitHost
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// Do not resolve arbitrary hostnames here; Apple Container plaintext
		// pushes are allowed only to local addresses emitted by Wendy helpers.
		return false
	}
	addr = addr.Unmap()
	return addr.IsLoopback()
}

func appleContainerPushScheme(registryImage string) (string, error) {
	if !registryImageUsesLoopbackRegistry(registryImage) {
		registry, _, _ := strings.Cut(registryImage, "/")
		return "", fmt.Errorf("Apple Container builder refuses plaintext push to non-loopback registry %q; use --builder docker", registry)
	}
	return "http", nil
}

// requireRegistryAuth checks whether the device's registry requires mTLS
// authentication and verifies the CLI has the necessary certs.
// Returns an error if the device is provisioned but no CLI certs are available.
func requireRegistryAuth(ctx context.Context, conn *grpcclient.AgentConnection) error {
	resp, err := conn.ProvisioningService.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
	if err != nil {
		return nil // can't determine provisioning status; let the push fail naturally
	}
	if _, ok := resp.GetResponse().(*agentpb.IsProvisionedResponse_Provisioned); ok {
		if loadCLICert() == nil {
			return fmt.Errorf("device is provisioned and its registry requires mTLS authentication.\nRun 'wendy auth login' to obtain client certificates before deploying")
		}
	}
	return nil
}

// detectProjectType determines the project type from the directory contents.
//
// Precedence: compose > Stagefile/Dockerfile/Containerfile > Package.swift > *.xcodeproj > Python markers > ESP-IDF markers.
// Returns an error only when multiple .xcodeproj directories are found.
func detectProjectType(dir string) (string, error) {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return "compose", nil
		}
	}
	if len(stagefile.SourceNames(dir)) > 0 {
		return "docker", nil
	}
	// Check base build files first (fast path), then any variant.
	for _, name := range []string{"Dockerfile", "Containerfile"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return "docker", nil
		}
	}
	if entries, readErr := os.ReadDir(dir); readErr == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if isContainerBuildFileName(name) {
				return "docker", nil
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "Package.swift")); err == nil {
		return "swift", nil
	}
	xp, err := findXcodeProj(dir)
	if err != nil {
		return "", err
	}
	if xp != "" {
		return "xcode", nil
	}
	if _, err := os.Stat(filepath.Join(dir, "requirements.txt")); err == nil {
		return "python", nil
	}
	if _, err := os.Stat(filepath.Join(dir, "setup.py")); err == nil {
		return "python", nil
	}
	if _, err := os.Stat(filepath.Join(dir, "pyproject.toml")); err == nil {
		return "python", nil
	}
	if espidftoolchain.IsEspIdfProject(dir) {
		return "esp-idf", nil
	}
	return "unknown", nil
}

// validDockerfileNameRe matches valid container build file names: "Dockerfile",
// "Containerfile", or either base name followed by a dot or hyphen and one or
// more safe characters.
var validDockerfileNameRe = regexp.MustCompile(`^(Dockerfile|Containerfile)([.\-][a-zA-Z0-9][a-zA-Z0-9._-]*)?$`)

// Device-reported build-arg hints (see applyDeviceBuildArgHints) are meant to be
// clean identifier/version tokens, never free text, so they keep a narrow
// allowlist. It lets an unmappable agent fallback like "L4T 38.2.0" (note the
// space) be dropped with a warning instead of injected as a bogus version.
// User-authored build args are NOT held to this — see validateBuildArgPair.
var validBuildArgHintValueRe = regexp.MustCompile(`^[A-Za-z0-9_.-]*$`)

func isContainerBuildFileName(name string) bool {
	if strings.HasSuffix(name, ".dockerignore") {
		return false
	}
	// Never a user-authored build file — it's the internal artifact
	// prepareDockerBuildFile writes and deliberately never deletes, so no
	// detection path (project-type, build options, provider fallback) may
	// treat it as a rival candidate on later runs.
	if isGeneratedBuildFileName(name) {
		return false
	}
	return validDockerfileNameRe.MatchString(name)
}

// preferredContainerBuildFileOption returns the build file a project builds when
// nobody chose one: the canonical Stagefile, else the conventional Dockerfile or
// Containerfile. A project whose only build files are variants
// ("prod.stagefile.yaml", "Dockerfile.prod") has no such default and returns
// nil, so the caller asks instead of guessing.
func preferredContainerBuildFileOption(options []BuildOption) *BuildOption {
	for _, preferred := range []string{stagefileSourceName, "Dockerfile", "Containerfile"} {
		for i := range options {
			if options[i].Type == "docker" && options[i].File == preferred {
				return &options[i]
			}
		}
	}
	return nil
}

func validateDockerfileName(name string) error {
	cleaned := filepath.Clean(name)
	if cleaned != filepath.Base(cleaned) {
		return fmt.Errorf("invalid container build file name %q: path separators are not allowed", name)
	}
	if strings.HasSuffix(cleaned, ".dockerignore") {
		return fmt.Errorf("invalid container build file name %q: .dockerignore files are not build files", cleaned)
	}
	// A Stagefile is a container build file too: --dockerfile is how a project
	// with several of them names one non-interactively (CI has no picker).
	if stagefile.IsSourceName(cleaned) {
		return nil
	}
	if !validDockerfileNameRe.MatchString(cleaned) {
		return fmt.Errorf("invalid container build file name %q: must be Dockerfile, Containerfile, a %s variant, or a dot/hyphen variant of either", cleaned, stagefileSourceName)
	}
	return nil
}

// validateBuildArgPair gates a KEY=VALUE build arg headed for a builder CLI.
// The rules live in shared/buildargs because the agent's remote build service
// must apply the same ones to what a client sends — see that package for why
// each rejection exists.
func validateBuildArgPair(key, value string) error {
	return buildargs.ValidatePair(key, value)
}

// applyDeviceBuildArgHints injects the optional device/GPU build-arg hints the
// agent reports (WENDY_DEVICE_TYPE, WENDY_HAS_GPU, WENDY_GPU_VENDOR,
// WENDY_JETPACK_VERSION, WENDY_JETPACK_MAJOR, WENDY_CUDA_VERSION) into
// buildArgs. Each hint is only set when the agent reports it, so Dockerfiles
// keep their own ARG defaults on older agents. These are meant to be clean
// identifier/version tokens, so a hint that isn't one (validBuildArgHintValueRe)
// is skipped with a warning rather than injected — e.g. a Jetson running an L4T
// release the agent's JetPack table doesn't map reports a fallback like
// "L4T 38.2.0", whose space fails the token check, so it is dropped and the
// Dockerfile keeps its ARG default instead of receiving a bogus version.
func applyDeviceBuildArgHints(buildArgs map[string]string, versionResp *agentpb.GetAgentVersionResponse) {
	setHint := func(key, value string) {
		if value == "" {
			return
		}
		// A hint must be a clean identifier/version token AND clear the build-arg
		// safety gate. Anything else — an unmappable "L4T 38.2.0" fallback, say —
		// is skipped with a warning rather than injected as a bogus hint or left to
		// abort the deploy at the final build-arg check.
		if !validBuildArgHintValueRe.MatchString(value) {
			cliLogln("Warning: ignoring device-reported build arg %s=%q: not a clean identifier/version token", key, value)
			return
		}
		if err := validateBuildArgPair(key, value); err != nil {
			cliLogln("Warning: ignoring device-reported build arg %s=%q: %v", key, value, err)
			return
		}
		buildArgs[key] = value
	}
	setHint("WENDY_DEVICE_TYPE", versionResp.GetDeviceType())
	// WENDY_HAS_GPU is a formatted bool, always allowlist-safe; only set it when
	// the optional field is present so older agents preserve the ARG default.
	if versionResp.HasGpu != nil {
		buildArgs["WENDY_HAS_GPU"] = fmt.Sprintf("%t", versionResp.GetHasGpu())
	}
	setHint("WENDY_GPU_VENDOR", versionResp.GetGpuVendor())
	setHint("WENDY_JETPACK_VERSION", versionResp.GetJetpackVersion())
	// Coarse major ("7" from "7.2") to aid in per-generation image selection
	setHint("WENDY_JETPACK_MAJOR", jetpackMajor(versionResp.GetJetpackVersion()))
	setHint("WENDY_CUDA_VERSION", versionResp.GetCudaVersion())
}

func jetpackMajor(version string) string {
	major, _, _ := strings.Cut(version, ".")
	if _, err := strconv.Atoi(major); err != nil {
		return "" // empty, or an unmapped "L4T 39.2.0" fallback — not a clean major
	}
	return major
}

func sortedValidatedBuildArgKeys(buildArgs map[string]string) ([]string, error) {
	return buildargs.SortedValidatedKeys(buildArgs)
}

// validComposeDockerfileNameRe matches the broader naming convention allowed by
// Docker Compose for the final path segment (e.g. "web.Dockerfile",
// "Containerfile", "Dockerfile.prod"). The allowlist rejects whitespace, shell
// metacharacters, and names starting with "-" that could be misread as CLI
// flags.
var validComposeDockerfileNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// validateComposeDockerfileName validates a dockerfile name sourced from a
// Compose service config. It applies a broader allowlist than validateDockerfileName
// to accommodate names like "web.Dockerfile" and "Containerfile". Subpaths
// (e.g. "build/web.Dockerfile") are accepted because Compose configs may point
// at a Dockerfile in a subdirectory; only the final path segment is regex-checked
// here, and confinement plus regular-file checks are enforced separately by
// confinedDockerfilePath.
func validateComposeDockerfileName(name string) error {
	base := filepath.Base(name)
	if !validComposeDockerfileNameRe.MatchString(base) {
		return fmt.Errorf("invalid compose dockerfile name %q: must start with a letter or digit and contain only letters, digits, dots, underscores, or hyphens", base)
	}
	return nil
}

// escapesBase reports whether a path returned by filepath.Rel walks outside
// the base directory. A plain prefix check on ".." is unsafe because directory
// names like "..cache" share that prefix without being a parent reference;
// only an exact ".." or a ".." segment followed by a separator counts as an
// escape.
func escapesBase(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// confinedDockerfilePath resolves dockerfile relative to base, uses
// filepath.EvalSymlinks on both the base and the joined path to neutralise
// symlink-based escapes, then verifies (via filepath.Rel) that the resolved
// target lies within base. Returns the resolved absolute path on success.
func confinedDockerfilePath(base, dockerfile string) (string, error) {
	absBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolving project directory: %w", err)
	}

	joined, err := filepath.Abs(filepath.Join(absBase, dockerfile))
	if err != nil {
		return "", fmt.Errorf("resolving dockerfile path: %w", err)
	}

	// Check containment before EvalSymlinks so that a non-existent path still
	// gets a clear "outside project" error rather than a confusing stat error.
	rel, err := filepath.Rel(absBase, joined)
	if err != nil || escapesBase(rel) {
		return "", fmt.Errorf("dockerfile %q must be within the project directory", dockerfile)
	}

	// Resolve symlinks and re-check to block symlink-based escapes.
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("dockerfile %q does not exist", dockerfile)
		}
		return "", fmt.Errorf("resolving dockerfile: %w", err)
	}
	rel, err = filepath.Rel(absBase, resolved)
	if err != nil || escapesBase(rel) {
		return "", fmt.Errorf("dockerfile %q must be within the project directory", dockerfile)
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("stating dockerfile: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("dockerfile %q is not a regular file", dockerfile)
	}

	return resolved, nil
}

// prepareDockerBuildFile makes sure dockerfile is ready to hand to the
// actual image builder, without ever modifying a real, user-authored
// Dockerfile on disk:
//
//   - If dockerfile is a Stagefile (any variant), it's compiled via the
//     stagefile package into that variant's generated Dockerfile (plus its
//     paired ".dockerignore") — see generatedBuildFileFor.
//   - Otherwise, if any purely-additive `wendy project optimize` fix
//     applies to it (see optimize.SafeAutoApplyFindings), the fixed text is
//     written to "Dockerfile.generated" and that name is returned instead.
//   - If neither applies, dockerfile is returned unchanged.
//
// Every branch writes into the "Dockerfile.generated*" namespace, which
// isGeneratedBuildFileName excludes from re-entering detection as a rival build
// file, so one exclusion covers all origins.
func prepareDockerBuildFile(dir, dockerfile, gpuArch string, sfOpts ...stagefile.Option) (string, error) {
	if stagefile.IsSourceName(dockerfile) {
		return compileStagefile(dir, dockerfile, gpuArch, sfOpts...)
	}
	return applySafeOptimizeFixes(dir, dockerfile)
}

// debugStagefileOptions is the Stagefile compile options implied by --debug:
// build swift and rust stages unoptimized so the deployed binary is debuggable.
// Without the flag it returns nothing, leaving whatever profile the Stagefile
// declares (which itself defaults to release) untouched.
func debugStagefileOptions(debug bool) []stagefile.Option {
	if !debug {
		return nil
	}
	return []stagefile.Option{stagefile.WithBuildProfile(stagefile.BuildProfileDebug)}
}

// frameworkStagefileOptions translates runtime framework configuration into
// compiler inputs. The project declares ROS 2 once in wendy.json; Stagefile
// then supplies the matching middleware package instead of making every
// service repeat Wendy's resolved distro/RMW choice in install.apt.
func frameworkStagefileOptions(frameworks *appconfig.FrameworksConfig) []stagefile.Option {
	if frameworks == nil || frameworks.ROS2 == nil {
		return nil
	}
	return ros2StagefileOptions(frameworks.ROS2)
}

func ros2StagefileOptions(ros2 *appconfig.ROS2Config) []stagefile.Option {
	if ros2 == nil {
		return nil
	}
	return []stagefile.Option{stagefile.WithROS2Runtime(ros2.ResolvedDistro(), ros2.ResolvedRMW())}
}

// hasContainerBuildFile reports whether dir holds any docker build source
// (Stagefile, Dockerfile/Containerfile, or a variant of either) without the
// compile / write side effects resolveDockerfile now has — for callers that
// only need existence, not a build-ready filename.
func hasContainerBuildFile(dir string) bool {
	if len(stagefile.SourceNames(dir)) > 0 {
		return true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && isContainerBuildFileName(e.Name()) {
			return true
		}
	}
	return false
}

// agentConn is nil-safe access to a selected device's agent connection, so
// callers that may not have resolved a device can still ask for one.
func agentConn(target *SelectedDevice) *grpcclient.AgentConnection {
	if target == nil {
		return nil
	}
	return target.Agent
}

// resolveGPUArch answers which GPU architecture a build in dir targets, or ""
// when the question doesn't arise.
//
// The device is the authority. A project deploys to whatever board is in front
// of it, so the architecture is a property of this build and not something a
// Stagefile could state without pinning itself to one board. flagArch is the
// escape hatch for building with no device attached (CI, plain `wendy build`).
//
// Nothing is asked of the device unless dir's Stagefile actually declares a
// cuda: stage — an ordinary build must not pay an RPC for a question it does
// not have.
func resolveGPUArch(ctx context.Context, dir, flagArch string, conn *grpcclient.AgentConnection) string {
	return resolveGPUArchForDirs(ctx, []string{dir}, flagArch, conn)
}

// resolveGPUArchForDirs is resolveGPUArch across several build contexts — the
// services of a multi-service or compose project, which share one device and
// therefore one architecture. The device is asked at most once no matter how
// many services there are, and not at all unless one of them declares cuda:.
func resolveGPUArchForDirs(ctx context.Context, dirs []string, flagArch string, conn *grpcclient.AgentConnection) string {
	if flagArch != "" {
		return flagArch
	}
	if conn == nil {
		return ""
	}
	needed := false
	for _, d := range dirs {
		if stagefile.NeedsGPUTarget(d) {
			needed = true
			break
		}
	}
	if !needed {
		return ""
	}
	resp, err := agentVersionForRun(ctx, conn)
	if err != nil {
		// Deliberately not fatal. The compiler is where "a GPU stage needs a
		// target" is enforced, and its error names the fix; failing here would
		// replace that with a transport error that doesn't.
		return ""
	}
	return resp.GetGpuArch()
}

// compileStagefile compiles the named Stagefile in dir into a real Dockerfile,
// writing that source's generated Dockerfile and paired .dockerignore into dir
// and returning the generated Dockerfile's filename.
func compileStagefile(dir, source, gpuArch string, sfOpts ...stagefile.Option) (string, error) {
	generatedName, generatedIgnoreName := generatedBuildFileFor(source)
	// A download with no sha256 in the Stagefile is pinned by fetching it
	// once, here, inline in the build. For model weights that is minutes of
	// silence on the first build, so say which URL is being pinned rather
	// than let it look like a hang.
	//
	// sfOpts comes last so a caller-supplied override (--debug's build profile)
	// wins over the defaults assembled here.
	opts := append([]stagefile.Option{
		stagefile.WithSource(source),
		stagefile.WithGPUArch(gpuArch),
		stagefile.WithProgress(func(url string) {
			cliNotice("pinning download %s (first build only; writes its sha256 to %s)", url, stagefile.LockName(source))
		}),
	}, sfOpts...)
	dockerfileText, dockerignoreText, err := stagefile.CompileFile(dir, "", opts...)
	if err != nil {
		return "", fmt.Errorf("compiling %s: %w", source, err)
	}
	if err := writeGeneratedFile(filepath.Join(dir, generatedName), []byte(dockerfileText)); err != nil {
		return "", fmt.Errorf("writing %s: %w", generatedName, err)
	}
	// BuildKit prefers <dockerfile>.dockerignore over .dockerignore for the
	// file passed via -f, so the derived allowlist rides along without ever
	// touching (or destroying) a user-authored .dockerignore in the project.
	//
	// That precedence is also why an empty derivation must leave no file behind
	// rather than write one: winning the precedence with a file that ignores
	// nothing would disable the project's own .dockerignore. An earlier build of
	// the same project may have written one, so remove it rather than assume
	// absence.
	ignorePath := filepath.Join(dir, generatedIgnoreName)
	if dockerignoreText == "" {
		if err := os.Remove(ignorePath); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("removing stale %s: %w", generatedIgnoreName, err)
		}
	} else if err := writeGeneratedFile(ignorePath, []byte(dockerignoreText)); err != nil {
		return "", fmt.Errorf("writing %s: %w", generatedIgnoreName, err)
	}
	return generatedName, nil
}

// applySafeOptimizeFixes runs the `wendy project optimize` analyzers against
// the Dockerfile at dir/dockerfile and, if any purely-additive fix applies
// (see optimize.SafeAutoApplyFindings — never npm-ci or a debug->release
// swap, both of which can change build outcome or behavior), writes the
// fixed text to "Dockerfile.generated" and returns that name. The real
// dockerfile on disk is never modified. Returns dockerfile unchanged if
// there's nothing to fix, or on any analysis error — this never blocks a
// build over a static-analysis problem.
func applySafeOptimizeFixes(dir, dockerfile string) (string, error) {
	cfg, _ := loadOptConfig(dir)
	targets, err := optimize.DiscoverTargets(dir, cfg, "arm64")
	if err != nil {
		return dockerfile, nil
	}
	dockerfilePath := filepath.Join(dir, dockerfile)
	var target *optimize.Target
	for i := range targets {
		if targets[i].Dockerfile != nil && targets[i].Dockerfile.Path == dockerfilePath {
			target = &targets[i]
			break
		}
	}
	if target == nil {
		return dockerfile, nil // e.g. a named variant like Dockerfile.prod — optimize doesn't scan those today.
	}

	findings := optimize.SafeAutoApplyFindings(optimize.Analyze([]optimize.Target{*target}, optimize.DefaultAnalyzers()))
	if len(findings) == 0 {
		return dockerfile, nil
	}
	fixedLines, applied := optimize.ApplyFixesToLines(target.Dockerfile.Lines, findings)
	anyApplied := false
	for _, a := range applied {
		if a.Applied {
			anyApplied = true
			break
		}
	}
	if !anyApplied {
		return dockerfile, nil
	}
	fixedText := strings.Join(fixedLines, "\n") + "\n"
	if err := writeGeneratedFile(filepath.Join(dir, generatedDockerfileName), []byte(fixedText)); err != nil {
		return dockerfile, nil
	}
	// A leftover Stagefile-derived allowlist (from a since-deleted
	// build.stagefile.yaml) would silently shrink this build's context via
	// BuildKit's per-Dockerfile ignore precedence — the auto-fixed copy must
	// build with the same ignore rules as the original Dockerfile.
	_ = os.Remove(filepath.Join(dir, generatedDockerignoreName))
	return generatedDockerfileName, nil
}

func resolveDockerfile(cwd, requested string, interactive bool, gpuArch string, sfOpts ...stagefile.Option) (string, error) {
	if requested != "" {
		if err := validateDockerfileName(requested); err != nil {
			return "", err
		}
		if _, err := confinedDockerfilePath(cwd, requested); err != nil {
			return "", err
		}
		// An explicitly named Dockerfile is handed to the builder verbatim —
		// "build exactly what I named" — but a Stagefile is not a Dockerfile, so
		// naming one still has to go through the compiler. Without this the
		// builder would receive the YAML source as its -f argument.
		//
		// sfOpts is forwarded here for the same reason the detection path below
		// forwards it: --debug has to reach a Stagefile the user named as much as
		// one the picker found, or `--dockerfile prod.stagefile.yaml --debug`
		// would quietly produce a release build.
		if stagefile.IsSourceName(requested) {
			return compileStagefile(cwd, requested, gpuArch, sfOpts...)
		}
		return requested, nil
	}

	var dockerfiles []BuildOption
	for _, opt := range detectBuildOptions(cwd) {
		if opt.Type == "docker" {
			dockerfiles = append(dockerfiles, opt)
		}
	}

	confine := func(file string) (string, error) {
		if _, err := confinedDockerfilePath(cwd, file); err != nil {
			return "", err
		}
		return prepareDockerBuildFile(cwd, file, gpuArch, sfOpts...)
	}

	if len(dockerfiles) <= 1 {
		if len(dockerfiles) == 1 {
			return confine(dockerfiles[0].File)
		}
		return "", nil
	}

	if !interactive {
		if preferred := preferredContainerBuildFileOption(dockerfiles); preferred != nil {
			file, err := confine(preferred.File)
			if err != nil {
				return "", err
			}
			cliNotice("multiple container build files detected; using %q. Use --dockerfile to select explicitly.", file)
			return file, nil
		}
		file, err := confine(dockerfiles[0].File)
		if err != nil {
			return "", err
		}
		cliNotice("multiple container build files detected; using %q. Use --dockerfile to select explicitly.", file)
		return file, nil
	}

	picked, err := pickBuildOptionWithTitle(dockerfiles, "Select a container build file")
	if err != nil {
		return "", err
	}
	return confine(picked.File)
}

// BuildOption represents a detected build type in a project directory.
type BuildOption struct {
	Label string // display name shown in the picker
	Type  string // build type key: "docker", "swift", "python"
	File  string // the marker filename (e.g. "Dockerfile.production", "Package.swift")
}

// detectBuildOptions finds all buildable project markers in the given directory.
// Unlike detectProjectType, this returns ALL options rather than the first match,
// including multiple container build files (Dockerfile, Containerfile, and variants).
func detectBuildOptions(dir string) []BuildOption {
	var options []BuildOption

	// Find compose files.
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			options = append(options, BuildOption{
				Label: name + " (Compose)",
				Type:  "compose",
				File:  name,
			})
			break
		}
	}

	// Stagefiles — the most specific, most intentional signal in a project
	// (nobody has one by accident), so detected ahead of any plain
	// Dockerfile/Containerfile that also happens to be present.
	//
	// Every variant is listed, not just the canonical build.stagefile.yaml.
	// Container build files have always been enumerated by pattern, so a project
	// with Dockerfile.prod/Dockerfile.gpu saw all of them in the picker while its
	// Stagefiles were matched by one hard-coded filename and stayed invisible.
	// SourceNames orders the family canonical-first.
	for _, name := range stagefile.SourceNames(dir) {
		options = append(options, BuildOption{
			Label: name + " (Stagefile)",
			Type:  "docker",
			File:  name,
		})
	}

	// Find all container build files.
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if isContainerBuildFileName(name) {
				options = append(options, BuildOption{
					Label: name,
					Type:  "docker",
					File:  name,
				})
			}
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "Package.swift")); err == nil {
		options = append(options, BuildOption{
			Label: "Package.swift (Swift)",
			Type:  "swift",
			File:  "Package.swift",
		})
	}

	// Xcode — one entry per .xcodeproj found (independent of Package.swift).
	if err == nil { // entries was read above
		for _, e := range entries {
			if e.IsDir() && strings.HasSuffix(e.Name(), ".xcodeproj") {
				options = append(options, BuildOption{
					Label: e.Name() + " (Xcode)",
					Type:  "xcode",
					File:  e.Name(),
				})
			}
		}
	}

	// Python — only add once even if multiple markers exist.
	for _, marker := range []string{"requirements.txt", "pyproject.toml", "setup.py"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			options = append(options, BuildOption{
				Label: marker + " (Python)",
				Type:  "python",
				File:  marker,
			})
			break
		}
	}

	return options
}

// injectDebugpy builds a wrapper image on top of the given image that installs debugpy.
func injectDebugpy(ctx context.Context, registryAddr, registryImage, platform string, buildArgs map[string]string, streamOutput io.Writer, useMTLS bool) error {
	tmpDir, err := os.MkdirTemp("", "wendy-debugpy-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	dockerfile := fmt.Sprintf("FROM %s\nUSER root\nRUN pip install debugpy\n", registryImage)
	if err := os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return fmt.Errorf("writing debugpy Dockerfile: %w", err)
	}

	return buildAndPushImage(ctx, tmpDir, registryAddr, registryImage, platform, "", buildArgs, "", streamOutput, streamOutput, useMTLS, nil)
}

func generatePythonDockerfile(dir string, debug bool) (string, error) {
	dockerfilePath := filepath.Join(dir, "Dockerfile")

	// Determine if requirements.txt exists.
	hasRequirements := false
	if _, err := os.Stat(filepath.Join(dir, "requirements.txt")); err == nil {
		hasRequirements = true
	}

	// Determine the entry point: look for app.py, main.py, or fall back.
	entryPoint := "app.py"
	for _, candidate := range []string{"app.py", "main.py"} {
		if _, err := os.Stat(filepath.Join(dir, candidate)); err == nil {
			entryPoint = candidate
			break
		}
	}

	var sb strings.Builder
	sb.WriteString("FROM python:3.11-slim\n")
	sb.WriteString("WORKDIR /app\n")
	if hasRequirements {
		sb.WriteString("COPY requirements.txt .\n")
		sb.WriteString("RUN pip install --no-cache-dir -r requirements.txt\n")
	}
	if debug {
		sb.WriteString("RUN pip install --no-cache-dir debugpy\n")
	}
	sb.WriteString("COPY . .\n")
	sb.WriteString(fmt.Sprintf("CMD [\"python\", \"%s\"]\n", entryPoint))

	if err := os.WriteFile(dockerfilePath, []byte(sb.String()), 0o644); err != nil {
		return "", fmt.Errorf("writing generated Dockerfile: %w", err)
	}

	return dockerfilePath, nil
}

func buildSwiftContainerImage(ctx context.Context, dir, product, registryAddr, architecture string, swiftUseMTLS, debug bool, toolchainStdout, toolchainStderr io.Writer) error {
	if err := ensureContainerPlugin(dir); err != nil {
		return err
	}

	sdk, err := swifttoolchain.FindSwiftSDK(ctx, architecture, toolchainStdout, toolchainStderr)
	if err != nil {
		return err
	}

	// Build release by default so on-device apps are optimized; debug only on
	// explicit opt-in (matches the native swift-build path). The container plugin
	// builds with `configuration: .inherit`, i.e. whatever `swift package` is told
	// here — without this it defaulted to debug, shipping unoptimized binaries
	// (e.g. a software renderer ran ~30x slower on device).
	buildConfig := "release"
	if debug {
		buildConfig = "debug"
	}

	// registryAddr is always a plain-HTTP address: either the device's own
	// unprovisioned registry or a local proxy that handles TLS on our behalf.
	swiftArgs := []string{
		"package",
		"-c", buildConfig,
		"--swift-sdk=" + sdk,
		"--allow-network-connections=all",
		"build-container-image",
		"--from=swift:" + swifttoolchain.DefaultVersion + "-slim",
		"--product=" + product,
		"--repository=" + registryAddr + "/" + strings.ToLower(product),
		"--architecture=" + architecture,
	}
	if !swiftUseMTLS {
		swiftArgs = append(swiftArgs, "--allow-insecure-http=destination")
	}

	cmd := swifttoolchain.SwiftCommandContext(ctx, swiftArgs...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("swift build-container-image failed: %w", err)
	}
	return nil
}

const containerPluginMinVersion = "1.3.0"

// ensureContainerPlugin checks that swift-container-plugin is available as a
// package plugin in the given project directory. If not, it automatically adds
// the dependency using `swift package add-dependency`.
func ensureContainerPlugin(dir string) error {
	cmd := swifttoolchain.SwiftCommand("package", "plugin", "--list")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to list Swift package plugins: %w", err)
	}

	if strings.Contains(string(out), "build-container-image") {
		return nil
	}

	fmt.Println("Adding swift-container-plugin dependency...")
	add := swifttoolchain.SwiftCommand("package", "add-dependency",
		"https://github.com/apple/swift-container-plugin", "--from", containerPluginMinVersion)
	add.Dir = dir
	add.Stdout = os.Stdout
	add.Stderr = os.Stderr
	if err := add.Run(); err != nil {
		return fmt.Errorf("failed to add swift-container-plugin dependency: %w", err)
	}

	return nil
}

type dockerRuntime struct {
	name        string
	app         string
	cliPaths    []string
	cliLinkHint string
}

type dockerHostOS string

const (
	dockerHostOSDarwin  dockerHostOS = "darwin"
	dockerHostOSWindows dockerHostOS = "windows"
)

// darwinDockerRuntimes lists macOS Docker-compatible runtimes in detection order.
// Each entry maps a human-readable name to its .app bundle path and known
// bundled Docker-compatible CLI locations.
var darwinDockerRuntimes = []dockerRuntime{
	{
		name: "OrbStack",
		app:  "/Applications/OrbStack.app",
		cliPaths: []string{
			"/Applications/OrbStack.app/Contents/MacOS/xbin/docker",
			"/Applications/OrbStack.app/Contents/Resources/bin/docker",
		},
		cliLinkHint: "install OrbStack's command-line tools or add its bundled docker CLI directory to PATH",
	},
	{
		name: "Docker Desktop",
		app:  "/Applications/Docker.app",
		cliPaths: []string{
			"/Applications/Docker.app/Contents/Resources/bin/docker",
		},
		cliLinkHint: "open Docker Desktop → Settings → Advanced → Command Line Tools and enable the Docker CLI symlink, or add /Applications/Docker.app/Contents/Resources/bin to PATH",
	},
	{
		name: "Rancher Desktop",
		app:  "/Applications/Rancher Desktop.app",
		cliPaths: []string{
			"/Applications/Rancher Desktop.app/Contents/Resources/resources/darwin/bin/docker",
		},
		cliLinkHint: "enable Rancher Desktop's Docker-compatible CLI integration or add its bundled docker CLI directory to PATH",
	},
}

// windowsDockerRuntimes lists Windows Docker-compatible runtimes whose
// installers normally add docker.exe to PATH, plus their bundled CLI locations
// for repairing PATH when the installer entry is missing from the environment.
var windowsDockerRuntimes = []dockerRuntime{
	{
		name: "Docker Desktop",
		app:  `C:\Program Files\Docker\Docker\Docker Desktop.exe`,
		cliPaths: []string{
			`C:\Program Files\Docker\Docker\resources\bin\docker.exe`,
		},
		cliLinkHint: `repair or reinstall Docker Desktop, or add C:\Program Files\Docker\Docker\resources\bin to PATH`,
	},
}

var (
	dockerLookPathFn    = exec.LookPath
	dockerStatFn        = os.Stat
	dockerVersionOKFn   = func(ctx context.Context) bool { return exec.CommandContext(ctx, "docker", "version").Run() == nil }
	dockerOpenRuntimeFn = func(ctx context.Context, appPath string) error {
		return exec.CommandContext(ctx, "open", "-a", appPath).Run()
	}
	dockerInstallRuntimeFn = func(ctx context.Context) error {
		installCmd := exec.CommandContext(ctx, "brew", "install", "--cask", "docker")
		installCmd.Stdout = os.Stdout
		installCmd.Stderr = os.Stderr
		return installCmd.Run()
	}
)

// ensureDockerDaemon verifies the Docker daemon is running. On macOS, when
// running interactively it prompts the user before launching the installed
// Docker runtime; in non-interactive mode it launches it automatically.
// Waits up to 60 s for the daemon to become ready before returning an error.
func ensureDockerDaemon(ctx context.Context) error {
	return ensureDockerDaemonForHostOS(ctx, dockerHostOS(runtime.GOOS))
}

func ensureDockerDaemonForHostOS(ctx context.Context, hostOS dockerHostOS) error {
	if dockerVersionOKFn(ctx) {
		return nil
	}

	_, cliErr := dockerLookPathFn("docker")
	cliOnPath := cliErr == nil

	if hostOS == dockerHostOSDarwin {
		rt, hasRuntime := detectDockerRuntimeInfoForHostOS(hostOS)
		if !cliOnPath {
			if !hasRuntime {
				if isInteractiveTerminalFn() {
					if !confirmFn("Docker runtime app and docker CLI were not found. Install Docker Desktop now with 'brew install --cask docker'?") {
						return fmt.Errorf("Docker runtime app is not installed — install Docker Desktop, OrbStack, or Rancher Desktop")
					}
					fmt.Fprintf(os.Stderr, "[docker] Installing Docker Desktop via Homebrew...\n")
					if err := dockerInstallRuntimeFn(ctx); err != nil {
						return fmt.Errorf("failed to install Docker Desktop: %w", err)
					}
					rt, hasRuntime = detectDockerRuntimeInfoForHostOS(hostOS)
				} else {
					return fmt.Errorf("Docker runtime app is not installed and docker CLI is not on PATH — install Docker Desktop, OrbStack, or Rancher Desktop")
				}
			}

			if hasRuntime {
				if cliRuntime, cliPath, ok := addBundledDockerCLIForInstalledRuntime(hostOS); ok {
					rt = cliRuntime
					fmt.Fprintf(os.Stderr, "[docker] docker CLI is not on PATH; using %s's bundled CLI at %s. To avoid this message: %s.\n", rt.name, cliPath, rt.cliLinkHint)
					cliOnPath = true
					if dockerVersionOKFn(ctx) {
						return nil
					}
				} else {
					return dockerCLIMissingError(rt)
				}
			}
		}

		if !hasRuntime {
			return fmt.Errorf("no supported Docker runtime app found — install Docker Desktop, OrbStack, or Rancher Desktop and try again")
		}
		if !cliOnPath {
			return dockerCLIMissingError(rt)
		}

		if isInteractiveTerminalFn() {
			if !confirmFn(fmt.Sprintf("Docker daemon is not running or is still starting for %s. Open it now?", rt.name)) {
				return fmt.Errorf("docker daemon is not running — please start %s and try again", rt.name)
			}
		}

		fmt.Fprintf(os.Stderr, "[docker] Opening %s...\n", rt.name)
		if err := dockerOpenRuntimeFn(ctx, rt.app); err != nil {
			return fmt.Errorf("docker daemon is not running: could not open %s: %w", rt.name, err)
		}
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			if dockerVersionOKFn(ctx) {
				fmt.Fprintf(os.Stderr, "[docker] %s is ready\n", rt.name)
				return nil
			}
		}
		return fmt.Errorf("docker daemon did not become ready within 60 seconds — %s may still be starting; please wait or start it manually", rt.name)
	}

	if hostOS == dockerHostOSWindows {
		rt, hasRuntime := detectDockerRuntimeInfoForHostOS(hostOS)
		if !cliOnPath {
			if hasRuntime {
				if cliRuntime, cliPath, ok := addBundledDockerCLIForInstalledRuntime(hostOS); ok {
					rt = cliRuntime
					fmt.Fprintf(os.Stderr, "[docker] docker CLI is not on PATH; using %s's bundled CLI at %s. To avoid this message: %s.\n", rt.name, cliPath, rt.cliLinkHint)
					cliOnPath = true
					if dockerVersionOKFn(ctx) {
						return nil
					}
				} else {
					return dockerCLIMissingError(rt)
				}
			} else {
				return fmt.Errorf("docker CLI is not on PATH — install Docker Desktop or add docker to PATH")
			}
		}
		if hasRuntime {
			return fmt.Errorf("docker daemon is not running — please start %s before using wendy", rt.name)
		}
	}

	if !cliOnPath {
		return fmt.Errorf("docker CLI is not on PATH — install Docker or add docker to PATH")
	}
	return fmt.Errorf("docker daemon is not running — please start Docker before using wendy")
}

func dockerCLIMissingError(rt dockerRuntime) error {
	return fmt.Errorf("%s is installed at %s, but docker CLI is not on PATH and Wendy could not find a bundled docker CLI. To fix: %s", rt.name, rt.app, rt.cliLinkHint)
}

func addBundledDockerCLIForInstalledRuntime(hostOS dockerHostOS) (dockerRuntime, string, bool) {
	for _, rt := range dockerRuntimesForHostOS(hostOS) {
		if !dockerRuntimeInstalled(rt) {
			continue
		}
		if cliPath, ok := addBundledDockerCLIToPath(rt); ok {
			return rt, cliPath, true
		}
	}
	return dockerRuntime{}, "", false
}

func addBundledDockerCLIToPath(rt dockerRuntime) (string, bool) {
	for _, cliPath := range rt.cliPaths {
		info, err := dockerStatFn(cliPath)
		if err != nil || info.IsDir() {
			continue
		}
		dir := filepath.Dir(cliPath)
		if !pathHasDir(os.Getenv("PATH"), dir) {
			path := dir
			if existing := os.Getenv("PATH"); existing != "" {
				path += string(filepath.ListSeparator) + existing
			}
			_ = os.Setenv("PATH", path)
		}
		return cliPath, true
	}
	return "", false
}

func pathHasDir(pathEnv, dir string) bool {
	for _, entry := range filepath.SplitList(pathEnv) {
		if entry == dir {
			return true
		}
	}
	return false
}

func detectDockerRuntime() (name, appPath string) {
	if rt, ok := detectDockerRuntimeInfoForHostOS(dockerHostOSDarwin); ok {
		return rt.name, rt.app
	}
	return "", ""
}

func detectDockerRuntimeInfo() (dockerRuntime, bool) {
	return detectDockerRuntimeInfoForHostOS(dockerHostOS(runtime.GOOS))
}

func detectDockerRuntimeInfoForHostOS(hostOS dockerHostOS) (dockerRuntime, bool) {
	for _, rt := range dockerRuntimesForHostOS(hostOS) {
		if dockerRuntimeInstalled(rt) {
			return rt, true
		}
	}
	return dockerRuntime{}, false
}

func dockerRuntimesForHostOS(hostOS dockerHostOS) []dockerRuntime {
	switch hostOS {
	case dockerHostOSDarwin:
		return darwinDockerRuntimes
	case dockerHostOSWindows:
		return windowsDockerRuntimes
	default:
		return nil
	}
}

func dockerRuntimeInstalled(rt dockerRuntime) bool {
	_, err := dockerStatFn(rt.app)
	return err == nil
}

// ensureBuildxBuilder ensures a buildx builder with the docker-container driver
// exists and returns its name plus the effective registry address to use in
// image references. For IPv6 addresses, a hostname alias is configured inside
// the builder container to avoid brackets that break the TOML parser.
func ensureBuildxBuilder(ctx context.Context, registryAddr string, useMTLS bool, w io.Writer) (builderName, effectiveAddr string, err error) {
	ensureBuildxBuilderMu.Lock()
	defer ensureBuildxBuilderMu.Unlock()

	if err := ensureDockerDaemon(ctx); err != nil {
		return "", "", err
	}
	// Use separate builders for mTLS and plaintext so switching between
	// provisioned and unprovisioned devices doesn't recreate builders.
	const containerCertDir = "/etc/buildkit/certs"

	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("finding home directory: %w", err)
	}
	configDir := filepath.Join(home, ".cache", "wendy")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return "", "", fmt.Errorf("creating config directory: %w", err)
	}

	// For IPv6 addresses (contain brackets), use a hostname alias to avoid
	// ']' in the TOML config — the go-toml v1 parser used by both docker
	// buildx and buildkitd rejects ']' in table-header keys.
	effectiveAddr, ipv6IP := splitIPv6RegistryAddr(registryAddr)

	if !useMTLS {
		builderName, err = ensurePlaintextBuilder(ctx, configDir, effectiveAddr, w)
	} else {
		builderName, err = ensureMTLSBuilder(ctx, configDir, effectiveAddr, containerCertDir, w)
	}
	if err != nil {
		return "", "", err
	}

	// Add a /etc/hosts entry inside the builder container so it can resolve
	// the alias to the real IPv6 address.
	if ipv6IP != "" {
		containerName := "buildx_buildkit_" + builderName + "0"
		hostsCmd := exec.CommandContext(ctx, "docker", "exec", containerName, "sh", "-c",
			fmt.Sprintf("if grep -q ' wendy-registry' /etc/hosts; then sed -i 's/^[^#]* wendy-registry$/%s wendy-registry/' /etc/hosts; else printf '\\n%s wendy-registry\\n' >> /etc/hosts; fi", ipv6IP, ipv6IP))
		if out, cmdErr := hostsCmd.CombinedOutput(); cmdErr != nil {
			return "", "", fmt.Errorf("adding hosts entry to builder: %s: %w", string(out), cmdErr)
		}
	}

	return builderName, effectiveAddr, nil
}

// ociBuilderName is the buildx builder used for OCI-layout export builds.
func ociBuilderName() string {
	base := os.Getenv("WENDY_BUILDX_BUILDER")
	if base == "" {
		base = "wendy"
	}
	return base + "-oci"
}

// ensureOCIExportBuilder ensures a dedicated buildx builder for OCI-layout
// export exists and is running, returning its name. Unlike the registry
// builders it needs NO registry config — OCI export never pushes to a registry
// and pulls base images over the public network — so once created it is reused
// across runs with only a lightweight bootstrap check. This avoids the per-run
// config-inject/restart cycle the registry builder pays because its buildkitd
// config embeds the per-run dynamic registry-proxy port (which changes every
// invocation, forcing a reconfigure each time).
func ensureOCIExportBuilder(ctx context.Context, w io.Writer) (string, error) {
	ensureBuildxBuilderMu.Lock()
	defer ensureBuildxBuilderMu.Unlock()

	builderName := ociBuilderName()

	// Fast path: if the builder's buildkit container is already running, then the
	// daemon is up, the builder exists, and it is bootstrapped — so we can skip
	// the `docker version` check and both `docker buildx inspect` calls, which
	// together cost ~200-280ms of pure verification overhead on every build. The
	// docker-container driver names the container buildx_buildkit_<name>0; a plain
	// `docker inspect` is far cheaper than `docker buildx inspect` (no buildx
	// plugin load, no buildkit gRPC handshake). On any miss we fall through to the
	// full robust path below.
	containerName := "buildx_buildkit_" + builderName + "0"
	if out, err := imageBuilderCommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", containerName).Output(); err == nil && strings.TrimSpace(string(out)) == "true" {
		return builderName, nil
	}

	if err := ensureDockerDaemon(ctx); err != nil {
		return "", err
	}

	exists := imageBuilderCommandContext(ctx, "docker", "buildx", "inspect", builderName).Run() == nil
	if !exists {
		fmt.Fprintf(w, "[buildx] creating OCI-export builder %q\n", builderName)
		cmd := imageBuilderCommandContext(ctx, "docker", "buildx", "create",
			"--name", builderName,
			"--driver", "docker-container",
			"--driver-opt", "network=host",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("creating buildx builder %q: %s: %w", builderName, string(out), err)
		}
	}

	// Ensure the builder is running and bootstrapped. This is cheap when it is
	// already up and, crucially, performs no config injection or container
	// restart; on a cold builder it pulls the BuildKit image inline, which
	// bootstrapOCIBuilder bounds and streams to w so the wait isn't silent.
	if err := bootstrapOCIBuilder(ctx, builderName, w); err != nil {
		return "", err
	}
	return builderName, nil
}

// ociBuilderBootstrapTimeout bounds `docker buildx inspect --bootstrap`: a cold
// start pulls the BuildKit image (minutes, not seconds), but a wedged daemon
// must not stall the deploy forever. Package var so tests can shrink it.
var ociBuilderBootstrapTimeout = 3 * time.Minute

// bootstrapOCIBuilder runs `docker buildx inspect --bootstrap` for builderName,
// streaming its output to w as it arrives — a cold builder pulls the BuildKit
// image inline with this call, so without live output the wait looks like a
// hang — while also buffering it for error context. ociBuilderBootstrapTimeout
// bounds the wait so a wedged Docker daemon cannot stall the deploy forever.
func bootstrapOCIBuilder(ctx context.Context, builderName string, w io.Writer) error {
	fmt.Fprintf(w, "[buildx] bootstrapping builder %q (a cold start pulls the BuildKit image; this can take a few minutes)\n", builderName)

	bootstrapCtx, cancel := context.WithTimeout(ctx, ociBuilderBootstrapTimeout)
	defer cancel()

	var buf bytes.Buffer
	// Stdout and Stderr are set to the *same* MultiWriter value (not two
	// separate io.MultiWriter calls) so os/exec recognizes them as equal and
	// combines them into a single pipe/copy goroutine — the same trick
	// CombinedOutput relies on — avoiding concurrent, unsynchronized writes
	// into buf from interleaved stdout/stderr output.
	mw := io.MultiWriter(w, &buf)
	cmd := imageBuilderCommandContext(bootstrapCtx, "docker", "buildx", "inspect", "--bootstrap", "--builder", builderName)
	cmd.Stdout = mw
	cmd.Stderr = mw
	if err := cmd.Run(); err != nil {
		// Only claim *our* bootstrap timeout fired if the parent ctx is still
		// alive: bootstrapCtx also reports DeadlineExceeded when an ancestor
		// context's own deadline expires first (ctx.Err() != nil in that case),
		// and blaming that on the bootstrap would misattribute the failure.
		// Plain Ctrl+C is unaffected — that propagates context.Canceled, not
		// DeadlineExceeded. (Same convention as buildprogress.go:135,173.)
		if errors.Is(bootstrapCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("bootstrapping builder %q timed out after %s (is the Docker daemon healthy?); output so far:\n%s",
				builderName, ociBuilderBootstrapTimeout, buf.String())
		}
		return fmt.Errorf("bootstrapping builder %q: %s: %w", builderName, buf.String(), err)
	}
	return nil
}

// buildkitRegistryConfig generates a buildkitd.toml snippet for the given
// registry address. IPv6 addresses must be passed through the hostname alias
// (e.g. "wendy-registry:5000") rather than in bracket notation, because the
// go-toml v1 parser used by buildkitd rejects ']' in table-header keys.
func buildkitRegistryConfig(registryAddr string, plainHTTP bool, keypair *[2]string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[registry.\"%s\"]\n", registryAddr)
	if plainHTTP {
		sb.WriteString("  http = true\n")
	}
	sb.WriteString("  insecure = true\n")
	if keypair != nil {
		fmt.Fprintf(&sb, "  [[registry.\"%s\".keypair]]\n", registryAddr)
		fmt.Fprintf(&sb, "    key = %q\n", keypair[0])
		fmt.Fprintf(&sb, "    cert = %q\n", keypair[1])
	}
	return sb.String()
}

// ensureRegistryBuilderInstance creates builderName only when it does not
// already exist. Registry configuration changes are deliberately not handled
// here: callers update the existing container in place so its BuildKit state
// volume survives dynamic proxy-port and certificate changes.
func ensureRegistryBuilderInstance(ctx context.Context, builderName string) (created bool, err error) {
	if exec.CommandContext(ctx, "docker", "buildx", "inspect", builderName).Run() == nil {
		return false, nil
	}
	cmd := exec.CommandContext(ctx, "docker", "buildx", "create",
		"--name", builderName,
		"--driver", "docker-container",
		"--driver-opt", "network=host",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("creating buildx builder %q: %s: %w", builderName, string(out), err)
	}
	return true, nil
}

// ensurePlaintextBuilder ensures the "wendy" buildx builder exists with plain
// HTTP registry config. The config is injected into the builder container via
// docker cp (not --buildkitd-config) to avoid the host-side TOML parser which
// cannot handle IPv6 brackets in registry addresses.
func ensurePlaintextBuilder(ctx context.Context, configDir, registryAddr string, w io.Writer) (string, error) {
	builderName := os.Getenv("WENDY_BUILDX_BUILDER")
	if builderName == "" {
		builderName = "wendy"
	}

	appliedPath := filepath.Join(configDir, builderName+".applied")

	fullConfig := buildkitRegistryConfig(registryAddr, true, nil)

	appliedConfig, _ := os.ReadFile(appliedPath)
	configChanged := string(appliedConfig) != fullConfig

	created, err := ensureRegistryBuilderInstance(ctx, builderName)
	if err != nil {
		return "", err
	}
	if created {
		configChanged = true // always inject config into a newly created builder
	}

	// Inject the real config into the existing builder container and restart only
	// when needed. In particular, do not remove/recreate the builder when the
	// per-run registry proxy port changes: its /var/lib/buildkit state is held in
	// a Docker volume, and an in-place restart preserves warm image snapshots.
	// Also re-inject if the container was destroyed (e.g. after colima restart) or
	// was bootstrapped without config injection (default buildkitd.toml lacks http=true).
	containerName := "buildx_buildkit_" + builderName + "0"

	// Read the config currently applied inside the running container (if any).
	var liveContainerConfig string
	if out, err := exec.CommandContext(ctx, "docker", "exec", containerName,
		"cat", "/etc/buildkit/buildkitd.toml").Output(); err == nil {
		liveContainerConfig = string(out)
	}

	if configChanged || liveContainerConfig != fullConfig {
		if err := updateBuilderConfig(ctx, builderName, fullConfig, w); err != nil {
			return "", fmt.Errorf("updating builder config: %w", err)
		}
		_ = os.WriteFile(appliedPath, []byte(fullConfig), 0o644)
	} else {
		// Builder exists with correct config — just ensure it's running.
		bootstrapCmd := exec.CommandContext(ctx, "docker", "buildx", "inspect", "--bootstrap", "--builder", builderName)
		if out, err := bootstrapCmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("bootstrapping builder: %s: %w", string(out), err)
		}
	}

	return builderName, nil
}

// ensureMTLSBuilder ensures the "wendy-mtls" buildx builder exists with mTLS
// client certs for the device registry.
func ensureMTLSBuilder(ctx context.Context, configDir, registryAddr, containerCertDir string, w io.Writer) (string, error) {
	base := os.Getenv("WENDY_BUILDX_BUILDER")
	if base == "" {
		base = "wendy"
	}
	builderName := base + "-mtls"

	appliedPath := filepath.Join(configDir, base+"-mtls.applied")

	certInfo := loadCLICert()
	if certInfo == nil || certInfo.PemCertificate == "" || !certInfo.HasPrivateKey() {
		return "", fmt.Errorf("mTLS connection but no CLI certificates available")
	}

	// Write cert files to host; they'll be docker-cp'd into the builder container.
	hostCertDir := filepath.Join(configDir, "certs")
	if err := os.MkdirAll(hostCertDir, 0o700); err != nil {
		return "", fmt.Errorf("creating cert directory: %w", err)
	}

	certPath := filepath.Join(hostCertDir, "client-cert.pem")
	keyPath := filepath.Join(hostCertDir, "client-key.pem")
	caPath := filepath.Join(hostCertDir, "ca.pem")

	// BuildKit and the agent registry both use Go's TLS stack, which parses
	// every certificate exchanged during the handshake even when verification
	// is disabled or custom. Wendy cloud chains can contain ML-DSA certificates
	// that Go cannot parse, so only present the parseable leaf certificate.
	leafCertPEM, err := certs.LeafCertificatePEM(certInfo.PemCertificate)
	if err != nil {
		return "", fmt.Errorf("extracting client leaf certificate: %w", err)
	}
	if err := os.WriteFile(certPath, []byte(leafCertPEM), 0o644); err != nil {
		return "", fmt.Errorf("writing client cert: %w", err)
	}
	keyPEM, err := certInfo.PrivateKeyPEM()
	if err != nil {
		return "", fmt.Errorf("loading client key: %w", err)
	}
	if err := os.WriteFile(keyPath, []byte(keyPEM), 0o600); err != nil {
		return "", fmt.Errorf("writing client key: %w", err)
	}
	if certInfo.PemCertificateChain != "" {
		if err := os.WriteFile(caPath, []byte(certInfo.PemCertificateChain), 0o644); err != nil {
			return "", fmt.Errorf("writing CA cert: %w", err)
		}
	}

	keypair := &[2]string{containerCertDir + "/client-key.pem", containerCertDir + "/client-cert.pem"}
	fullConfig := buildkitRegistryConfig(registryAddr, false, keypair)

	// appliedState uses the buildkitd config plus a SHA-256 digest of the
	// public leaf certificate. The certificate is public material; no private
	// key material or derivative is persisted (SOC2-C1, NIST-SC-28, ISO27001-A.8).
	// When the cert changes (rotation, new device), the digest changes and the
	// builder is reconfigured and restarted in place.
	certDigest := sha256.Sum256([]byte(leafCertPEM))
	appliedState := fullConfig +
		"\n---CERTHASH---\n" + hex.EncodeToString(certDigest[:])

	appliedConfig, _ := os.ReadFile(appliedPath)
	configChanged := string(appliedConfig) != appliedState

	created, err := ensureRegistryBuilderInstance(ctx, builderName)
	if err != nil {
		return "", err
	}
	if created {
		configChanged = true // always inject certs and config into a newly created builder
	}

	// Only copy certs and restart the builder when something actually changed.
	// Reconfigure an existing builder in place so its /var/lib/buildkit volume —
	// and therefore its warm image snapshots — survives per-run proxy-port changes.
	// Restarting while another parallel build uses the same builder kills that build.
	if configChanged {
		if err := copyCertsToBuilder(ctx, builderName, hostCertDir, containerCertDir); err != nil {
			return "", fmt.Errorf("copying certs to builder: %w", err)
		}
		if err := updateBuilderConfig(ctx, builderName, fullConfig, w); err != nil {
			return "", fmt.Errorf("updating builder config: %w", err)
		}
		_ = os.WriteFile(appliedPath, []byte(appliedState), 0o600)
	} else {
		// Builder exists with correct config — just ensure it's running.
		bootstrapCmd := exec.CommandContext(ctx, "docker", "buildx", "inspect", "--bootstrap", "--builder", builderName)
		if out, err := bootstrapCmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("bootstrapping builder: %s: %w", string(out), err)
		}
	}

	return builderName, nil
}

// copyCertsToBuilder bootstraps the buildx builder container and copies TLS
// client certificates from the host into it so buildkitd can authenticate
// with the device registry.
func copyCertsToBuilder(ctx context.Context, builderName, hostCertDir, containerCertDir string) error {
	// Bootstrap the builder to ensure the container is running.
	cmd := exec.CommandContext(ctx, "docker", "buildx", "inspect", "--bootstrap", "--builder", builderName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bootstrapping builder: %s: %w", string(out), err)
	}

	// The docker-container driver names the container buildx_buildkit_<name>0.
	containerName := "buildx_buildkit_" + builderName + "0"

	// Copy cert files into the running container.
	cmd = exec.CommandContext(ctx, "docker", "cp", hostCertDir+"/.", containerName+":"+containerCertDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker cp certs: %s: %w", string(out), err)
	}

	return nil
}

// updateBuilderConfig bootstraps the buildx builder container (if not already
// running), writes a new buildkitd.toml into it, and restarts so the updated
// configuration takes effect.
func updateBuilderConfig(ctx context.Context, builderName, config string, w io.Writer) error {
	fmt.Fprintf(w, "[buildx] bootstrapping builder %q\n", builderName)
	bootstrapCmd := exec.CommandContext(ctx, "docker", "buildx", "inspect", "--bootstrap", "--builder", builderName)
	if out, err := bootstrapCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bootstrapping builder: %s: %w", string(out), err)
	}
	fmt.Fprintf(w, "[buildx] bootstrap done\n")

	containerName := "buildx_buildkit_" + builderName + "0"
	const containerConfigPath = "/etc/buildkit/buildkitd.toml"

	// Write config to a temp file, then docker-cp it in.
	tmp, err := os.CreateTemp("", "buildkitd-*.toml")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(config); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp config: %w", err)
	}
	tmp.Close()

	fmt.Fprintf(w, "[buildx] copying config into container %q\n", containerName)
	cmd := exec.CommandContext(ctx, "docker", "cp", tmp.Name(), containerName+":"+containerConfigPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker cp config: %s: %w", string(out), err)
	}

	// Inject a clean Docker config without credsStore so buildkitd (running on
	// Linux inside Colima) does not call docker-credential-osxkeychain, which
	// is a macOS binary that does not exist on Linux and causes "signal: killed"
	// errors when pulling public base images (e.g. python:3.11-slim).
	fmt.Fprintf(w, "[buildx] injecting clean docker config into container %q\n", containerName)
	injectCmd := exec.CommandContext(ctx, "docker", "exec", containerName,
		"sh", "-c", `mkdir -p /root/.docker && printf '{"auths":{}}' > /root/.docker/config.json`)
	if out, err := injectCmd.CombinedOutput(); err != nil {
		// Non-fatal: log the error but proceed. The credential helper may still
		// fail for private images, but public images will work without credentials.
		fmt.Fprintf(w, "[buildx] warning: could not inject docker config: %s\n", string(out))
	} else {
		fmt.Fprintf(w, "[buildx] docker config injected\n")
	}

	fmt.Fprintf(w, "[buildx] restarting container %q\n", containerName)
	cmd = exec.CommandContext(ctx, "docker", "restart", containerName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restarting builder: %s: %w", string(out), err)
	}
	fmt.Fprintf(w, "[buildx] container restarted, waiting for buildkitd\n")

	bootstrapAfterRestart := exec.CommandContext(ctx, "docker", "buildx", "inspect", "--bootstrap", "--builder", builderName)
	if out, err := bootstrapAfterRestart.CombinedOutput(); err != nil {
		return fmt.Errorf("waiting for builder after restart: %s: %w", string(out), err)
	}
	fmt.Fprintf(w, "[buildx] builder ready\n")

	return nil
}

// buildxLocalCacheDir returns the local buildx cache directory
// (--cache-to/--cache-from type=local) for a build. A non-empty cacheKey gives
// the build its own isolated subdir so concurrent multi-service builds never
// share one cache dir — BuildKit's local cache-export ingest store is not safe
// for concurrent writers and parallel builds clobber each other's temp files
// (WDY-1689). An empty cacheKey uses the shared base dir so single and
// sequential builds keep their cross-run cache. Used by the registry-push build
// path; the OCI-export path embeds its cache inline in the layout instead.
func buildxLocalCacheDir(userCache, cacheKey string) string {
	dir := filepath.Join(userCache, "wendy", "buildx")
	if cacheKey != "" {
		dir = filepath.Join(dir, buildxCacheSubdir(cacheKey))
	}
	return dir
}

// buildxCacheSubdirNameLimit caps the readable part of a cache subdir name so
// long service names cannot push the full cache path past filesystem limits.
const buildxCacheSubdirNameLimit = 48

// buildxCacheSubdir maps a build's cache key to one filesystem-safe path element
// that is unique per key. The readable prefix keeps the cache browsable
// (…/buildx/ros2multi-talker-<sha256>) while the full digest suffix makes
// collisions computationally infeasible: sanitization alone folds distinct
// service names together ("a/b" and "a-b", "Talker" and "talker"), and a
// collision silently restores the concurrent-ingest corruption this isolation
// exists to prevent (WDY-1711). The suffix also keeps the element from ever
// being "." or ".." — service names are not schema-constrained, and a bare ".."
// would place the cache outside its root.
func buildxCacheSubdir(cacheKey string) string {
	sum := sha256.Sum256([]byte(cacheKey))
	name := sanitizeAppleContainerTag(cacheKey)
	if len(name) > buildxCacheSubdirNameLimit {
		name = name[:buildxCacheSubdirNameLimit]
	}
	return name + "-" + hex.EncodeToString(sum[:])
}

// cleanDockerConfigContents is the credential-helper-free Docker config the
// build paths point DOCKER_CONFIG at.
const cleanDockerConfigContents = `{"auths":{}}`

// ensureCleanDockerConfig materializes a Docker config directory without a
// credsStore credential helper and returns its path. It returns "" on Windows,
// where the host's own config is used unchanged.
//
// On macOS the default config has "credsStore":"osxkeychain". When docker buildx
// forwards credentials to buildkitd via the build session, it calls the
// credential helper on the host. In CI (launchd agent context), the Keychain is
// inaccessible and the helper is killed → "signal: killed". Public images (e.g.
// python:3.11-slim) need no credentials; anonymous pull works fine with an empty
// auths map.
//
// On Windows, Docker Desktop's credential helper is always available and
// symlinks for builder-state lookup are unreliable in elevated processes, so we
// skip this override entirely and let docker use its normal config.
//
// The config file is written atomically (temp file + rename) and only when its
// contents differ. Concurrent service builds (the multibuild parallel path) all
// call this while other builds' buildx clients are reading the very same file,
// and a plain truncate-then-write briefly exposes an empty config to those
// readers — one more piece of shared build state that must be concurrency-safe
// (WDY-1711).
func ensureCleanDockerConfig() (string, error) {
	if runtime.GOOS == "windows" {
		return "", nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding home directory: %w", err)
	}
	dir := filepath.Join(home, ".cache", "wendy", "docker-config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating clean docker config directory: %w", err)
	}

	configFile := filepath.Join(dir, "config.json")
	if existing, err := os.ReadFile(configFile); err != nil || string(existing) != cleanDockerConfigContents {
		tmp, err := os.CreateTemp(dir, "config.json.*")
		if err != nil {
			return "", fmt.Errorf("writing clean docker config: %w", err)
		}
		tmpName := tmp.Name()
		_, writeErr := tmp.WriteString(cleanDockerConfigContents)
		if closeErr := tmp.Close(); writeErr == nil {
			writeErr = closeErr
		}
		if writeErr == nil {
			writeErr = os.Chmod(tmpName, 0o644)
		}
		if writeErr == nil {
			writeErr = os.Rename(tmpName, configFile)
		}
		if writeErr != nil {
			os.Remove(tmpName)
			return "", fmt.Errorf("writing clean docker config: %w", writeErr)
		}
	}

	// Symlink subdirs that docker/buildx need to find plugins and builder state.
	origDockerConfig := os.Getenv("DOCKER_CONFIG")
	if origDockerConfig == "" {
		origDockerConfig = filepath.Join(home, ".docker")
	}
	for _, subdir := range []string{"buildx", "cli-plugins", "contexts"} {
		dst := filepath.Join(dir, subdir)
		if _, err := os.Lstat(dst); err != nil {
			// best-effort: ignore if source doesn't exist or symlink fails
			_ = os.Symlink(filepath.Join(origDockerConfig, subdir), dst)
		}
	}

	return dir, nil
}

// dockerConfigEnv returns the build command's environment with DOCKER_CONFIG
// pointed at configDir, or nil when configDir is empty (Windows) and the
// process environment should be inherited unchanged.
func dockerConfigEnv(configDir string) []string {
	if configDir == "" {
		return nil
	}
	baseEnv := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "DOCKER_CONFIG=") {
			baseEnv = append(baseEnv, e)
		}
	}
	return append(baseEnv, "DOCKER_CONFIG="+configDir)
}

// dialErr, when non-nil, exposes the registry proxy's most recent failure to
// reach the device registry; a refused dial short-circuits the retry loop.
func buildAndPushImage(ctx context.Context, dir, registryAddr, registryImage, platform, dockerfile string, buildArgs map[string]string, cacheKey string, streamOutput, logOutput io.Writer, useMTLS bool, dialErr func() error) error {
	// Serialize against other wendy processes: the buildx builder is shared, and
	// reconfiguring or restarting it mid-build kills a concurrent build (#1017).
	// Concurrent builds within this process share the lock via reference counting.
	releaseLock, err := buildLock.acquire(ctx, logOutput)
	if err != nil {
		return err
	}
	defer releaseLock()

	builder, effectiveAddr, err := ensureBuildxBuilder(ctx, registryAddr, useMTLS, logOutput)
	if err != nil {
		return err
	}

	// When an IPv6 alias is in use, rewrite the image reference to match.
	if effectiveAddr != registryAddr {
		registryImage = strings.Replace(registryImage, registryAddr, effectiveAddr, 1)
	}

	// Use UserCacheDir so the cache lives in the platform's idiomatic location:
	// %LOCALAPPDATA% on Windows, ~/Library/Caches on macOS, $XDG_CACHE_HOME (or
	// ~/.cache) on Linux.
	userCache, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("finding user cache directory: %w", err)
	}
	cacheDir := buildxLocalCacheDir(userCache, cacheKey)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("creating cache directory: %w", err)
	}

	cleanDockerConfigDir, err := ensureCleanDockerConfig()
	if err != nil {
		return err
	}

	// buildkitd inside the Linux VM appends "/index.json" to the cache src/dest,
	// so pass forward-slash paths to avoid mixed-separator warnings on Windows.
	cacheDirSlash := filepath.ToSlash(cacheDir)
	args := []string{
		"buildx", "build",
		"--builder", builder,
		"--platform", platform,
		"--progress", buildxProgressMode(ctx),
	}
	if dockerfile != "" {
		// Callers validate the filename at their own boundary: the CLI flag path
		// uses the strict validateDockerfileName, and Compose uses the broader
		// validateComposeDockerfileName so names like "Containerfile" and
		// "web.Dockerfile" pass through. confinedDockerfilePath enforces
		// containment and regular-file checks and yields an absolute path, which
		// docker buildx -f cannot misinterpret as a flag.
		resolvedDockerfile, err := confinedDockerfilePath(dir, dockerfile)
		if err != nil {
			return err
		}
		args = append(args, "-f", resolvedDockerfile)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "index.json")); err == nil {
		args = append(args, "--cache-from", "type=local,src="+cacheDirSlash)
	}
	args = append(args, "--cache-to", "type=local,dest="+cacheDirSlash)
	// Sort keys so the argument order is stable across runs, which keeps
	// build logs reproducible and avoids flakiness in tests that assert args.
	keys, err := sortedValidatedBuildArgKeys(buildArgs)
	if err != nil {
		return err
	}
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+buildArgs[k])
	}
	args = append(args,
		"--output", "type=image,name="+registryImage+",push=true",
		".",
	)

	// Build the command environment once; it is reused across retry attempts.
	// On macOS/Linux, override DOCKER_CONFIG so the buildx client does not
	// call the host credential helper when setting up the build session.
	// On Windows we leave DOCKER_CONFIG untouched (cleanDockerConfigDir == "").
	cmdEnv := dockerConfigEnv(cleanDockerConfigDir)

	fmt.Fprintf(logOutput, "[buildx] starting build: docker %s\n", strings.Join(redactBuildArgsForLog(args), " "))

	// Build and push, retrying transient registry/push failures. Images push to
	// the device registry through one shared mTLS tunnel; under concurrent
	// multi-service load it can briefly collapse (TLS handshake timeouts on even
	// cheap blob HEADs) and buildkit reports the whole build as failed though the
	// device is healthy (WDY-1690). A retry is cheap: the build is a cache hit and
	// buildkit re-pushes only the blobs that did not make it.
	var lastErr error
	for attempt := 1; attempt <= maxBuildPushAttempts; attempt++ {
		capture := &capturingWriter{w: streamOutput}
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Dir = dir
		cmd.Stdout = capture
		cmd.Stderr = capture
		if cmdEnv != nil {
			cmd.Env = cmdEnv
		}

		err := cmd.Run()
		if err == nil {
			return nil
		}
		lastErr = err
		var lastDial error
		if dialErr != nil {
			lastDial = dialErr()
		}
		if !shouldRetryPush(ctx.Err(), attempt, capture.String(), lastDial) {
			if isDialRefused(lastDial) {
				fmt.Fprintf(logOutput, "[buildx] device registry refused the connection; not retrying\n")
			}
			break
		}
		backoff := buildPushRetryBackoff(attempt)
		fmt.Fprintf(logOutput, "[buildx] transient registry/push error; retrying in %s (attempt %d/%d)\n", backoff, attempt+1, maxBuildPushAttempts)
		select {
		case <-ctx.Done():
			return fmt.Errorf("docker buildx build failed: %w", lastErr)
		case <-time.After(backoff):
		}
	}
	return fmt.Errorf("docker buildx build failed: %w", lastErr)
}

// maxBuildPushAttempts bounds how many times a fused buildx build+push is retried
// on a transient registry/push failure (WDY-1690).
const maxBuildPushAttempts = 3

// shouldRetryPush decides whether a failed buildx build+push attempt is worth
// retrying. Cancellation, the final attempt, and non-transient output never
// retry; neither does a registry that actively refused the proxy's dial —
// nothing is listening there, so every retry would fail identically.
func shouldRetryPush(ctxErr error, attempt int, output string, dialErr error) bool {
	if ctxErr != nil || attempt >= maxBuildPushAttempts {
		return false
	}
	if isDialRefused(dialErr) {
		return false
	}
	return isTransientPushError(output)
}

// buildPushRetryBackoff returns the wait before retry N+1 (2s, 4s). The tunnel
// recovers quickly once concurrent push pressure drops, so a short linear backoff
// is enough.
func buildPushRetryBackoff(attempt int) time.Duration {
	return time.Duration(attempt) * 2 * time.Second
}

// transientPushErrorRe matches buildkit output for registry/push failures that
// are worth retrying — the device-registry tunnel collapsing under concurrent
// pushes surfaces as TLS handshake timeouts and push failures, not as a genuine
// build error (WDY-1690).
var transientPushErrorRe = regexp.MustCompile(`(?i)(tls handshake timeout|failed to push|failed to do request|connection reset by peer|i/o timeout|unexpected eof|broken pipe|write: connection timed out|503 service unavailable|429 too many requests)`)

func isTransientPushError(output string) bool {
	return transientPushErrorRe.MatchString(output)
}

// capturingWriter tees writes to an underlying writer while retaining the last
// maxCaptureBytes bytes, so a failed build's tail can be classified for the
// push-retry path (WDY-1690) without buffering the entire (large) build log.
type capturingWriter struct {
	w   io.Writer
	buf []byte
}

const maxCaptureBytes = 64 << 10

func (c *capturingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.buf = append(c.buf, p[:n]...)
	if len(c.buf) > maxCaptureBytes {
		c.buf = append([]byte(nil), c.buf[len(c.buf)-maxCaptureBytes:]...)
	}
	return n, err
}

func (c *capturingWriter) String() string { return string(c.buf) }

func buildAndPushImageWithBuilder(ctx context.Context, builder, dir, registryAddr, registryImage, platform, dockerfile string, buildArgs map[string]string, cacheKey string, streamOutput, logOutput io.Writer, useMTLS bool, dialErr func() error) error {
	normalized, err := normalizeImageBuilder(builder)
	if err != nil {
		return err
	}
	switch normalized {
	case imageBuilderDocker:
		return buildAndPushImage(ctx, dir, registryAddr, registryImage, platform, dockerfile, buildArgs, cacheKey, streamOutput, logOutput, useMTLS, dialErr)
	case imageBuilderAppleContainer:
		return buildAndPushImageWithAppleContainer(ctx, dir, registryImage, platform, dockerfile, buildArgs, streamOutput, logOutput, useMTLS)
	default:
		return fmt.Errorf("unsupported image builder %q", normalized)
	}
}

func buildAndPushImageForAgent(ctx context.Context, conn *grpcclient.AgentConnection, regPort int, agentOS, builder, dir, repo, platform, dockerfile string, buildArgs map[string]string, cacheKey string, streamOutput, logOutput io.Writer) error {
	if _, err := normalizeImageBuilder(builder); err != nil {
		return err
	}
	if imageBuilderWasExplicit(builder) {
		return buildAndPushImageForAgentWithBuilder(ctx, conn, regPort, agentOS, builder, dir, repo, platform, dockerfile, buildArgs, cacheKey, streamOutput, logOutput)
	}
	if shouldAutoAttemptAppleContainerBuilder() {
		// Apple Container builds don't use buildx, so the local-cache key never
		// applies; only the Docker fallback below consumes it. The auto-attempt path
		// must not prompt or start services as a side effect: if Apple Container is
		// not already ready, fall back to Docker. Use --builder apple-container to
		// require Apple Container and get the startup prompt.
		if err := checkAppleContainerBuilder(ctx); err != nil {
			logAppleContainerFallback(logOutput, err)
		} else if err := buildAndPushImageForAgentWithBuilder(ctx, conn, regPort, agentOS, imageBuilderAppleContainer, dir, repo, platform, dockerfile, buildArgs, "", streamOutput, logOutput); err == nil {
			return nil
		} else if isRegistryUnavailable(err) {
			// The device registry refuses connections; the Docker fallback pushes
			// to the same registry and would fail identically — surface the
			// actionable error instead of burning a full rebuild.
			return err
		} else {
			logAppleContainerFallback(logOutput, err)
		}
	}
	return buildAndPushImageForAgentWithBuilder(ctx, conn, regPort, agentOS, imageBuilderDocker, dir, repo, platform, dockerfile, buildArgs, cacheKey, streamOutput, logOutput)
}

func buildAndPushImageForAgentWithBuilder(ctx context.Context, conn *grpcclient.AgentConnection, regPort int, agentOS, builder, dir, repo, platform, dockerfile string, buildArgs map[string]string, cacheKey string, streamOutput, logOutput io.Writer) error {
	normalized, err := normalizeImageBuilder(builder)
	if err != nil {
		return err
	}

	// Docker deployments build through the stable OCI-export builder and push
	// from the host. BuildKit therefore never receives a deployment's temporary
	// registry address, so one `wendy run` cannot reconfigure/restart the shared
	// builder underneath another. Independent app/service layouts and local
	// caches are keyed separately and may build concurrently; the same key is
	// serialized by lockOCILayoutDir.
	if normalized == imageBuilderDocker {
		registryAddr, useMTLS, cleanup, dialErr, resolveErr := resolveRegistryForSwiftAgent(ctx, conn, regPort)
		if resolveErr != nil {
			return resolveErr
		}
		defer cleanup()

		if err := buildAndPushImageViaOCILayout(ctx, dir, registryAddr, repo, platform, dockerfile, buildArgs, cacheKey, streamOutput, logOutput, useMTLS); err != nil {
			return maybeRegistryUnavailable(agentOS, conn.Host, dialErr, err)
		}
		return nil
	}

	registryAddr, cleanup, useMTLS, dialErr, err := resolveRegistryForImageBuilder(ctx, conn, regPort, normalized)
	if err != nil {
		return err
	}
	defer cleanup()

	registryImage := fmt.Sprintf("%s/%s:latest", registryAddr, strings.ToLower(repo))
	if err := buildAndPushImageWithBuilder(ctx, normalized, dir, registryAddr, registryImage, platform, dockerfile, buildArgs, cacheKey, streamOutput, logOutput, useMTLS, dialErr); err != nil {
		return maybeRegistryUnavailable(agentOS, conn.Host, dialErr, err)
	}
	return nil
}

// buildAndPushImageViaOCILayout is the concurrency-safe Docker deployment
// path. A stable, registry-agnostic BuildKit builder writes a persistent OCI
// layout; the host then pushes that image to the device. Per-app/service locks
// protect the lazy OCI files while allowing unrelated deployments to overlap.
func buildAndPushImageViaOCILayout(ctx context.Context, dir, registryAddr, repo, platform, dockerfile string, buildArgs map[string]string, cacheKey string, streamOutput, logOutput io.Writer, useMTLS bool) error {
	userCache, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("finding user cache directory: %w", err)
	}

	repo = strings.ToLower(repo)
	layoutDir := chunkLayoutDir(userCache, repo, platform)
	releaseLayout, err := lockOCILayoutDir(ctx, layoutDir)
	if err != nil {
		return err
	}
	defer releaseLayout()
	defer func() { _ = gcOCILayoutDir(layoutDir) }()

	native, err := buildOrUpdateOCILayout(dir, dockerfile, platform, buildArgs, layoutDir, func() error {
		return buildImageToOCILayoutDirWithDocker(ctx, dir, dockerfile, platform, buildArgs, layoutDir, streamOutput, logOutput)
	})
	if err != nil {
		return err
	}
	if native {
		fmt.Fprintln(logOutput, "[stagefile] app layer(s) resolved natively; buildx and cache export skipped")
	}

	fmt.Fprintf(logOutput, "[registry] pushing OCI image to %s/%s:latest\n", registryAddr, repo)
	if err := pushOCILayoutToRegistry(ctx, layoutDir, platform, registryAddr, repo, useMTLS); err != nil {
		return err
	}
	return nil
}

// buildAndPrepareComposeImageForAgent is the default Compose image path. It
// builds into the same persistent OCI layout used by single-service chunk
// deploys, updates Stagefile app layers natively when dependencies are stable,
// and transfers missing content-defined chunks straight to the agent. The
// prepared image is registered under repo:latest, so Compose's existing
// CreateContainer orchestration can consume it without a registry round-trip.
//
// Agents that predate PrepareImage (or any other non-build chunk-path failure)
// fall back to pushing this exact already-built layout to the device registry;
// the image is never rebuilt for the fallback.
func buildAndPrepareComposeImageForAgent(ctx context.Context, conn *grpcclient.AgentConnection, regPort int, agentOS, builder, dir, repo, platform, dockerfile string, buildArgs map[string]string, cacheKey string, streamOutput, logOutput io.Writer) error {
	return buildAndPrepareComposeImage(ctx, conn, regPort, agentOS, builder, dir, repo, platform, dockerfile, buildArgs, cacheKey, streamOutput, logOutput, true)
}

func buildAndPrepareComposeImageForAgentForce(ctx context.Context, conn *grpcclient.AgentConnection, regPort int, agentOS, builder, dir, repo, platform, dockerfile string, buildArgs map[string]string, cacheKey string, streamOutput, logOutput io.Writer) error {
	return buildAndPrepareComposeImage(ctx, conn, regPort, agentOS, builder, dir, repo, platform, dockerfile, buildArgs, cacheKey, streamOutput, logOutput, false)
}

func buildAndPrepareComposeImage(ctx context.Context, conn *grpcclient.AgentConnection, regPort int, agentOS, builder, dir, repo, platform, dockerfile string, buildArgs map[string]string, cacheKey string, streamOutput, logOutput io.Writer, allowRegistryFallback bool) error {
	normalized, err := normalizeImageBuilder(builder)
	if err != nil {
		return err
	}
	if normalized != imageBuilderDocker {
		return buildAndPushImageForAgent(ctx, conn, regPort, agentOS, builder, dir, repo, platform, dockerfile, buildArgs, cacheKey, streamOutput, logOutput)
	}

	userCache, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("finding user cache directory: %w", err)
	}
	repo = strings.ToLower(repo)
	layoutDir := chunkLayoutDir(userCache, repo, platform)
	releaseLayout, err := lockOCILayoutDir(ctx, layoutDir)
	if err != nil {
		return err
	}
	defer releaseLayout()
	defer func() { _ = gcOCILayoutDir(layoutDir) }()

	native, err := buildOrUpdateOCILayout(dir, dockerfile, platform, buildArgs, layoutDir, func() error {
		return buildImageToOCILayoutDirWithDocker(ctx, dir, dockerfile, platform, buildArgs, layoutDir, streamOutput, logOutput)
	})
	if err != nil {
		return err
	}
	if native {
		fmt.Fprintln(logOutput, "[stagefile] app layer(s) resolved natively; buildx and cache export skipped")
	}

	layers, imageConfig, err := readOCILayoutDirLayers(layoutDir, platform)
	if err != nil {
		return err
	}
	if reporter, ok := streamOutput.(interface{ ReportImageContent([]string) }); ok {
		diffIDs := make([]string, 0, len(layers))
		for _, layer := range layers {
			if layer.DiffID == "" {
				diffIDs = nil
				break
			}
			diffIDs = append(diffIDs, layer.DiffID)
		}
		if len(diffIDs) > 0 {
			reporter.ReportImageContent(diffIDs)
		}
	}
	imageSignature, err := readOptionalSignature(os.Getenv(imageSignaturePathEnv))
	if err != nil {
		return fmt.Errorf("reading image signature from %s: %w", imageSignaturePathEnv, err)
	}
	if reporter, ok := streamOutput.(interface{ ReportImagePreparationQueued() }); ok {
		reporter.ReportImagePreparationQueued()
	}
	releasePrepare, err := acquireComposePrepare(ctx)
	if err != nil {
		return err
	}
	defer releasePrepare()
	// Match the reference Compose passes to CreateContainer below. PrepareImage
	// records this exact name in containerd even though no registry is involved.
	imageName := fmt.Sprintf("localhost:%d/%s:latest", regPort, repo)
	prepare := func(prepareCtx context.Context, headers []*agentpb.RunContainerLayerHeader) error {
		_, prepareErr := conn.ContainerService.PrepareImage(prepareCtx, &agentpb.RunContainerLayersRequest{
			ImageName:      imageName,
			Layers:         headers,
			ImageConfig:    imageConfig,
			ImageSignature: imageSignature,
		})
		return prepareErr
	}
	if _, chunkErr := pushLayersByChunksWithStrictPrepareOutput(ctx, conn.ContainerService, layers, prepare, streamOutput); chunkErr == nil {
		fmt.Fprintf(logOutput, "[chunks] prepared %s from missing content\n", imageName)
		return nil
	} else if ctx.Err() != nil {
		return chunkErr
	} else if blocksChunkPrepareFallback(chunkErr) {
		return chunkErr
	} else if !allowRegistryFallback {
		return fmt.Errorf("chunk-diff image preparation failed and --chunking=force disables the registry fallback: %w", chunkErr)
	} else {
		fmt.Fprintf(logOutput, "[chunks] prepare unavailable (%v); falling back to registry push of the existing OCI layout\n", chunkErr)
		if reporter, ok := streamOutput.(interface{ ReportRegistryFallback(error) }); ok {
			reporter.ReportRegistryFallback(chunkErr)
		}
	}

	registryAddr, useMTLS, cleanup, dialErr, resolveErr := resolveRegistryForSwiftAgent(ctx, conn, regPort)
	if resolveErr != nil {
		return resolveErr
	}
	defer cleanup()
	if err := pushOCILayoutToRegistry(ctx, layoutDir, platform, registryAddr, repo, useMTLS); err != nil {
		return maybeRegistryUnavailable(agentOS, conn.Host, dialErr, err)
	}
	return nil
}

func buildAndPushImageWithAppleContainer(ctx context.Context, dir, registryImage, platform, dockerfile string, buildArgs map[string]string, streamOutput, logOutput io.Writer, useMTLS bool) error {
	if useMTLS {
		return fmt.Errorf("Apple Container builder cannot push directly to an mTLS registry; use --builder docker or connect through a local registry proxy")
	}
	if err := checkAppleContainerBuilder(ctx); err != nil {
		return err
	}
	if err := buildImageWithAppleContainer(ctx, dir, registryImage, platform, dockerfile, buildArgs, streamOutput, logOutput); err != nil {
		return err
	}

	scheme, err := appleContainerPushScheme(registryImage)
	if err != nil {
		return err
	}
	args := []string{"image", "push", "--scheme", scheme, "--platform", platform, registryImage}
	fmt.Fprintf(logOutput, "[apple-container] pushing image: container %s\n", strings.Join(args, " "))
	cmd := imageBuilderCommandContext(ctx, "container", args...)
	cmd.Stdout = streamOutput
	cmd.Stderr = streamOutput
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("container image push failed: %w", err)
	}
	return nil
}

func buildImageWithAppleContainer(ctx context.Context, dir, imageName, platform, dockerfile string, buildArgs map[string]string, streamOutput, logOutput io.Writer) error {
	buildContext, err := appleContainerBuildContextPath(dir)
	if err != nil {
		return fmt.Errorf("resolving project path: %w", err)
	}
	contextMonitor := newAppleContainerBuildContextMonitor(buildContext)
	streamOutput = contextMonitor.wrapStream(streamOutput)
	// --progress plain emits the deterministic BuildKit log format (#N, DONE Ns,
	// [stage N/M]) that the shared build parser understands; the default
	// (--progress auto) renders an interactive [+] Building UI the parser cannot
	// read, so the renderer would show nothing.
	args := []string{"build", "--progress", "plain", "--platform", platform, "-t", imageName}
	if dockerfile != "" {
		resolvedDockerfile, err := appleContainerBuildFilePath(dir, dockerfile)
		if err != nil {
			return err
		}
		args = append(args, "-f", resolvedDockerfile)
	}
	keys, err := sortedValidatedBuildArgKeys(buildArgs)
	if err != nil {
		return err
	}
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+buildArgs[k])
	}
	args = append(args, buildContext)

	fmt.Fprintf(logOutput, "[apple-container] starting build: container %s\n", strings.Join(redactBuildArgsForLog(args), " "))
	cmd := imageBuilderCommandContext(ctx, "container", args...)
	cmd.Dir = buildContext
	cmd.Stdout = streamOutput
	cmd.Stderr = streamOutput
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("container build failed: %w", contextMonitor.wrapBuildError(err))
	}
	return nil
}

func checkAppleContainerBuilder(ctx context.Context) error {
	if err := checkAppleContainerCLI(ctx); err != nil {
		return err
	}
	return appleContainerSystemStatus(ctx)
}

// checkAppleContainerCLI verifies the host can run the Apple Container CLI:
// Apple silicon, the `container` binary on PATH, and a usable `--version`.
func checkAppleContainerCLI(ctx context.Context) error {
	if imageBuilderHostGOOS() != "darwin" || imageBuilderHostGOARCH() != "arm64" {
		return fmt.Errorf("Apple Container builder requires an Apple silicon Mac")
	}
	if _, err := imageBuilderLookPath("container"); err != nil {
		return fmt.Errorf("container CLI is not installed or not in PATH")
	}
	if err := imageBuilderCommandContext(ctx, "container", "--version").Run(); err != nil {
		return fmt.Errorf("container CLI is not usable: %w", err)
	}
	return nil
}

// appleContainerSystemStatus reports whether the Apple Container system
// (apiserver) is running, returning a descriptive error when it is not.
func appleContainerSystemStatus(ctx context.Context) error {
	cmd := imageBuilderCommandContext(ctx, "container", "system", "status")
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := safeCommandOutputSummary(out, 256)
		if msg != "" {
			msg = ": " + msg
		}
		return fmt.Errorf("Apple Container system is not running%s. Run 'container system start' and try again: %w", msg, err)
	}
	return nil
}

// appleContainerStartTimeout bounds how long we wait for the Apple Container
// system to become ready after `container system start`.
// appleContainerStatusPollInterval is how often readiness is re-checked.
// Both are vars so tests can shrink them.
var (
	appleContainerStartTimeout       = 60 * time.Second
	appleContainerStatusPollInterval = 2 * time.Second
)

// ensureAppleContainerSystem verifies the Apple Container system is running and
// offers to start it when it is not. It is called both on explicit
// `--builder apple-container` paths and on the no-builder auto-attempt paths
// (Apple silicon), which now prefer Apple Container whenever its CLI is
// available and start the system on demand rather than silently falling back to
// Docker. Callers fall back to Docker when this returns an error (CLI
// unavailable, user declined, or the system failed to start).
//
// When the system is not running and we are attached to an interactive terminal,
// the user is prompted before starting. assumeYes (from --yes, and implicitly
// `wendy watch`) skips the prompt and starts automatically, as does a
// non-interactive invocation.
func ensureAppleContainerSystem(ctx context.Context, assumeYes bool) error {
	if err := checkAppleContainerCLI(ctx); err != nil {
		return err
	}
	if appleContainerSystemStatus(ctx) == nil {
		return nil
	}

	if isInteractiveTerminalFn() && !assumeYes {
		if !confirmFn("Apple Container system is not running. Start it now?") {
			return appleContainerSystemStatus(ctx)
		}
	}

	fmt.Fprintln(os.Stderr, "[apple-container] Starting Apple Container system...")
	startCmd := imageBuilderCommandContext(ctx, "container", "system", "start", "--timeout", "60")
	startOut, startErr := startCmd.CombinedOutput()

	deadline := time.Now().Add(appleContainerStartTimeout)
	for {
		if appleContainerSystemStatus(ctx) == nil {
			fmt.Fprintln(os.Stderr, "[apple-container] Apple Container system is ready")
			return nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(appleContainerStatusPollInterval):
		}
	}

	msg := safeCommandOutputSummary(startOut, 256)
	if msg != "" {
		msg = ": " + msg
	}
	if startErr != nil {
		return fmt.Errorf("could not start Apple Container system%s: %w", msg, startErr)
	}
	return fmt.Errorf("Apple Container system did not become ready within %s%s; run 'container system start' manually and check 'container system status'", appleContainerStartTimeout, msg)
}

// ensureAppleContainerSystemForBuilder runs ensureAppleContainerSystem only when
// the builder was explicitly set to apple-container. The no-builder auto-attempt
// selection (no --builder, on Apple silicon) calls ensureAppleContainerSystem
// directly at its decision point, so this helper is a no-op for it. Safe to call
// from any build path: it no-ops unless the builder is explicit apple-container.
func ensureAppleContainerSystemForBuilder(ctx context.Context, builder string, assumeYes bool) error {
	if !imageBuilderWasExplicit(builder) {
		return nil
	}
	normalized, err := normalizeImageBuilder(builder)
	if err != nil {
		return err
	}
	if normalized != imageBuilderAppleContainer {
		return nil
	}
	return ensureAppleContainerSystem(ctx, assumeYes)
}

func appleContainerBuildContextPath(projectPath string) (string, error) {
	buildContext, err := filepath.Abs(projectPath)
	if err != nil {
		return "", err
	}
	if imageBuilderHostGOOS() == "darwin" {
		if normalized, ok := appleContainerTmpAlias(buildContext); ok {
			return normalized, nil
		}
	}
	return buildContext, nil
}

func appleContainerBuildFilePath(projectPath, dockerfile string) (string, error) {
	resolved, err := confinedDockerfilePath(projectPath, dockerfile)
	if err != nil {
		return "", err
	}
	if imageBuilderHostGOOS() == "darwin" {
		if normalized, ok := appleContainerTmpAlias(resolved); ok {
			return normalized, nil
		}
	}
	return resolved, nil
}

func appleContainerTmpAlias(path string) (string, bool) {
	const privateTmp = "/private/tmp"
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	if canonical != privateTmp && !strings.HasPrefix(canonical, privateTmp+"/") {
		return "", false
	}
	candidate := "/tmp" + strings.TrimPrefix(canonical, privateTmp)
	candidateCanonical, err := filepath.EvalSymlinks(candidate)
	if err != nil || candidateCanonical != canonical {
		return "", false
	}
	return candidate, true
}

func resolveRegistryForImageBuilder(ctx context.Context, conn *grpcclient.AgentConnection, port int, builder string) (registryAddr string, cleanup func(), useMTLS bool, dialErr func() error, err error) {
	normalized, err := normalizeImageBuilder(builder)
	if err != nil {
		return "", nil, false, nil, err
	}
	switch normalized {
	case imageBuilderDocker:
		registryAddr, cleanup, dialErr, err = resolveRegistryForAgent(ctx, conn, port)
		return registryAddr, cleanup, conn.IsMTLS, dialErr, err
	case imageBuilderAppleContainer:
		return resolveRegistryForAppleContainer(ctx, conn, port)
	default:
		return "", nil, false, nil, fmt.Errorf("unsupported image builder %q", normalized)
	}
}

// registryHost formats a host:port for use in a registry image reference,
// wrapping IPv6 addresses in brackets as required by RFC 3986.
// If the host is a hostname (not an IP), it is resolved to an IP address first
// so that Docker buildx (which runs inside a VM with its own DNS) can reach the
// device registry even when the hostname is only resolvable via mDNS or
// Tailscale DNS on the host machine.
//
// IPv6 link-local addresses (fe80::/10) contain a zone ID (e.g. %en0) that is
// meaningful only on the host machine and cannot be used inside a Docker
// buildkit container. For literal IP inputs the zone ID is stripped; for
// hostnames the resolver prefers routable IPv4 or global IPv6 addresses.
func registryHost(host string, port int) string {
	host = resolveRegistryIP(host)
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// resolveRegistry determines how to reach the device registry from Docker buildx.
// Buildkitd runs inside the Docker VM (Colima/Docker Desktop) and cannot reach
// LAN devices directly — only the macOS host can. We always proxy through
// host.docker.internal so buildkitd can push via the host regardless of whether
// the address is link-local or a routable LAN IP.
//
// The returned cleanup function MUST be called when the build is complete to
// stop the proxy and release the port. dialErr reports the proxy's most recent
// failure to reach the device registry (nil when there is no proxy or the last
// attempt succeeded); it is non-nil on every nil-error return.
func resolveRegistry(ctx context.Context, host string, port int) (registryAddr string, cleanup func(), dialErr func() error, err error) {
	resolved := resolveRegistryIP(host)

	// On Linux, buildkitd uses host networking (--driver-opt network=host) and
	// can reach LAN devices directly. No proxy needed, and host.docker.internal
	// does not exist on Linux.
	if runtime.GOOS == "linux" {
		addr := resolved
		if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
			addr = "[" + addr + "]"
		}
		return fmt.Sprintf("%s:%d", addr, port), func() {}, func() error { return nil }, nil
	}

	// On macOS, buildkitd runs inside the Colima VM and cannot reach LAN devices
	// directly. Proxy through host.docker.internal so the VM reaches the macOS host,
	// which then forwards to the device.
	//
	// For link-local addresses (USB devices), dial via the original hostname so
	// the host's resolver supplies the zone ID needed for link-local routing.
	// For routable LAN addresses, dial the resolved IP directly.
	var target string
	if isLinkLocalIP(resolved) {
		target = net.JoinHostPort(host, strconv.Itoa(port))
	} else {
		target = net.JoinHostPort(resolved, strconv.Itoa(port))
	}

	// Bind loopback only; the Docker VM forwards host.docker.internal to it.
	proxy, err := startRegistryProxy(ctx, registryProxyListenAddr, target)
	if err != nil {
		return "", nil, nil, fmt.Errorf("starting registry proxy: %w", err)
	}

	registryAddr = fmt.Sprintf("host.docker.internal:%d", proxy.Port())
	return registryAddr, proxy.Close, proxy.LastDialError, nil
}

// ensureBuildxBuilderMu serializes builder creation so that concurrent service
// builds don't race on "docker buildx create --name <builder>": the second
// caller would fail with "existing instance for … but no append mode".
var ensureBuildxBuilderMu sync.Mutex

// registryProxyEntry is the cached result of resolving a device registry for
// one AgentConnection: the stable proxy address plus the proxy's dial-error
// accessor, so every build sharing the proxy can also inspect why pushes
// through it are failing (e.g. a Mac agent with no container runtime).
type registryProxyEntry struct {
	addr    string
	dialErr func() error
}

// dockerRegistryProxyAddrs caches one proxy entry per AgentConnection. The
// proxy is allocated once (port 0 → OS-assigned) and reused for all pushes on
// that connection, so the buildx builder config never changes between concurrent
// builds and no builder teardown races can kill an in-flight push.
var (
	dockerRegistryProxyCacheMu sync.Mutex
	dockerRegistryProxyAddrs   = map[*grpcclient.AgentConnection]registryProxyEntry{}
)

// resolveRegistryForAgent determines how Docker buildx should reach the
// agent's registry. The proxy is started once per connection and cached so
// concurrent pushes to the same device share a stable host:port address.
// dialErr reports the shared proxy's most recent failure to reach the device
// registry; it is non-nil on every nil-error return.
func resolveRegistryForAgent(ctx context.Context, conn *grpcclient.AgentConnection, port int) (registryAddr string, cleanup func(), dialErr func() error, err error) {
	port, err = registryHostPortForAgent(ctx, conn, port)
	if err != nil {
		return "", nil, nil, err
	}
	// Hold the lock for the entire operation so concurrent callers block rather
	// than each starting their own proxy. Proxy creation is just a local
	// net.Listen call, so the lock is held only briefly.
	dockerRegistryProxyCacheMu.Lock()
	defer dockerRegistryProxyCacheMu.Unlock()

	if entry, ok := dockerRegistryProxyAddrs[conn]; ok {
		return entry.addr, func() {}, entry.dialErr, nil
	}

	// Start a proxy tied to context.Background so it outlives this push and is
	// reused by subsequent pushes on the same connection.
	var addr string
	var stopProxy func()

	if conn.RegistryDialer == nil {
		addr, stopProxy, dialErr, err = resolveRegistry(context.Background(), conn.Host, port)
		if err != nil {
			return "", nil, nil, err
		}
	} else {
		// Bind loopback only; the Docker VM forwards host.docker.internal to it.
		proxy, proxyErr := startRegistryProxyWithDialer(context.Background(), registryProxyListenAddr, func(ctx context.Context) (net.Conn, error) {
			return conn.RegistryDialer(ctx, port)
		})
		if proxyErr != nil {
			return "", nil, nil, fmt.Errorf("starting cloud registry proxy: %w", proxyErr)
		}
		stopProxy = proxy.Close
		dialErr = proxy.LastDialError
		if runtime.GOOS == "linux" {
			addr = fmt.Sprintf("127.0.0.1:%d", proxy.Port())
		} else {
			addr = fmt.Sprintf("host.docker.internal:%d", proxy.Port())
		}
	}

	dockerRegistryProxyAddrs[conn] = registryProxyEntry{addr: addr, dialErr: dialErr}
	conn.ExtraClosers = append(conn.ExtraClosers, closeFunc(func() {
		dockerRegistryProxyCacheMu.Lock()
		delete(dockerRegistryProxyAddrs, conn)
		dockerRegistryProxyCacheMu.Unlock()
		stopProxy()
	}))

	return addr, func() {}, dialErr, nil
}

// resolveRegistryForSwift is like resolveRegistry but for the Swift container
// plugin, which runs on the host (not inside a Docker VM). Because the host
// can resolve mDNS hostnames directly, we pass the original hostname through
// rather than resolving it to an IP. Only link-local addresses (USB) still
// need the TCP proxy.
func resolveRegistryForSwift(ctx context.Context, host string, port int) (registryAddr string, cleanup func(), dialErr func() error, err error) {
	resolved := resolveRegistryIP(host)
	if !isLinkLocalIP(resolved) {
		// Use the original hostname (or bare IP) directly — mDNS-resolvable on the host.
		addr := host
		if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
			addr = "[" + addr + "]"
		}
		return fmt.Sprintf("%s:%d", addr, port), func() {}, func() error { return nil }, nil
	}

	// Link-local: proxy via 127.0.0.1 — Swift runs on the host, not in a VM.
	target := net.JoinHostPort(host, strconv.Itoa(port))
	proxy, err := startRegistryProxy(ctx, "127.0.0.1:0", target)
	if err != nil {
		return "", nil, nil, fmt.Errorf("starting registry proxy for link-local device: %w", err)
	}
	return fmt.Sprintf("127.0.0.1:%d", proxy.Port()), proxy.Close, proxy.LastDialError, nil
}

// resolveRegistryForSwiftAgent resolves a registry address for the Swift
// container plugin to push to, alongside a dialErr accessor: after a push
// fails, callers can call dialErr() to check whether the underlying proxy
// (if any) ever failed to reach its target — e.g. connection refused because
// a Mac agent has no container runtime listening — and surface that instead
// of a generic build failure. dialErr is always safely callable (never nil)
// on a nil-error return.
func resolveRegistryForSwiftAgent(ctx context.Context, conn *grpcclient.AgentConnection, port int) (registryAddr string, swiftUseMTLS bool, cleanup func(), dialErr func() error, err error) {
	port, err = registryHostPortForAgent(ctx, conn, port)
	if err != nil {
		return "", false, nil, nil, err
	}
	if conn.RegistryDialer == nil {
		if conn.IsMTLS {
			// Provisioned LAN device: the registry speaks HTTPS with a cert signed
			// by the Wendy Cloud Root CA, which is not in the macOS system keychain.
			// Stand up a local HTTP reverse proxy that terminates TLS with mTLS so
			// the Swift container plugin can push via plain HTTP on 127.0.0.1.
			certInfo := loadCLICert()
			if certInfo == nil {
				return "", false, nil, nil, fmt.Errorf("mTLS connection but no CLI certificates available")
			}
			target := net.JoinHostPort(conn.Host, strconv.Itoa(port))
			keyPEM, keyErr := certInfo.PrivateKeyPEM()
			if keyErr != nil {
				return "", false, nil, nil, fmt.Errorf("loading client key: %w", keyErr)
			}
			proxy, proxyErr := startMTLSRegistryHTTPProxy(target, certInfo.PemCertificate, keyPEM, certInfo.PemCertificateChain)
			if proxyErr != nil {
				return "", false, nil, nil, fmt.Errorf("starting mTLS registry proxy for Swift: %w", proxyErr)
			}
			return fmt.Sprintf("127.0.0.1:%d", proxy.Port()), false, proxy.Close, proxy.LastDialError, nil
		}
		addr, addrCleanup, addrDialErr, addrErr := resolveRegistryForSwift(ctx, conn.Host, port)
		return addr, false, addrCleanup, addrDialErr, addrErr
	}

	// Cloud tunnel. On a provisioned device the registry itself speaks TLS,
	// but registry tooling (containertool, Apple Container) treats a
	// 127.0.0.1 registry as plain HTTP — and the device's Wendy-CA cert
	// could never verify for the loopback address anyway. So terminate TLS
	// in the proxy: upgrade each tunneled connection with the CLI's client
	// certificate and hand the plugin a plain-HTTP loopback address
	// (swiftUseMTLS=false), mirroring the provisioned-LAN branch above.
	// Previously the raw TCP forward returned swiftUseMTLS=true, so the
	// plugin sent plain HTTP straight into the TLS listener and every
	// tunnel deploy to an enrolled device hung on GET /v2/ (WDY-1868).
	dial := func(ctx context.Context) (net.Conn, error) {
		return conn.RegistryDialer(ctx, port)
	}
	if conn.IsMTLS {
		certInfo := loadCLICert()
		if certInfo == nil {
			return "", false, nil, nil, fmt.Errorf("mTLS connection but no CLI certificates available")
		}
		keyPEM, keyErr := certInfo.PrivateKeyPEM()
		if keyErr != nil {
			return "", false, nil, nil, fmt.Errorf("loading client key: %w", keyErr)
		}
		tlsDial, tlsErr := tlsClientDialer(certInfo.PemCertificate, keyPEM, certInfo.PemCertificateChain, dial)
		if tlsErr != nil {
			return "", false, nil, nil, fmt.Errorf("preparing TLS dialer for tunneled registry: %w", tlsErr)
		}
		dial = tlsDial
	}

	proxy, proxyErr := startRegistryProxyWithDialer(ctx, "127.0.0.1:0", dial)
	if proxyErr != nil {
		return "", false, nil, nil, fmt.Errorf("starting cloud registry proxy for Swift: %w", proxyErr)
	}
	return fmt.Sprintf("127.0.0.1:%d", proxy.Port()), false, proxy.Close, proxy.LastDialError, nil
}

// wendyRegistryServerVerifier returns a VerifyConnection callback that
// chain-validates the registry server's certificate against the Wendy CA.
//
// Wendy issues a single mutual-auth identity cert per principal, used for
// mTLS in both directions; when a device serves its registry it presents
// that identity cert, which carries clientAuth (mirrored by the agent-side
// verifier in agent/mtls) and NOT serverAuth. Requiring serverAuth therefore
// rejects a legitimately trusted device cert, so we accept either
// authentication EKU — but still require an authentication cert (rejecting
// e.g. codeSigning/emailProtection leaves) and full chain validation against
// the Wendy CA. The residual exposure (a clientAuth identity-cert holder
// impersonating the registry) requires MITM of the loopback-only proxy's
// connection to the device and is identical to the gRPC channel's trust
// model; the long-term fix is issuing device registry certs with a
// serverAuth EKU at the PKI layer.
func wendyRegistryServerVerifier(caPool *x509.CertPool) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("server presented no certificates")
		}
		intermediates := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			intermediates.AddCert(c)
		}
		opts := x509.VerifyOptions{
			Roots:         caPool,
			Intermediates: intermediates,
			KeyUsages: []x509.ExtKeyUsage{
				x509.ExtKeyUsageServerAuth,
				x509.ExtKeyUsageClientAuth,
			},
		}
		_, err := cs.PeerCertificates[0].Verify(opts)
		return err
	}
}

// tlsClientDialer wraps dial so every connection is upgraded to TLS with the
// given CLI client certificate before use. Hostname verification is skipped —
// device registry certs never carry the dialed (loopback/tunnel) address as a
// SAN — but the server chain is fully validated against the Wendy CA via
// wendyRegistryServerVerifier, matching startMTLSRegistryHTTPProxy.
func tlsClientDialer(certPEM, keyPEM, caPEM string, dial func(context.Context) (net.Conn, error)) (func(context.Context) (net.Conn, error), error) {
	leafPEM, err := certs.LeafCertificatePEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("extracting leaf certificate: %w", err)
	}
	tlsCert, err := tls.X509KeyPair([]byte(leafPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("loading client certificate: %w", err)
	}
	caPool := x509.NewCertPool()
	if certs.AppendChainToPool(caPool, caPEM) == 0 {
		return nil, fmt.Errorf("no valid CA certificates found in caPEM")
	}
	tlsCfg := &tls.Config{
		// Hostname check only; full chain validation happens in VerifyConnection.
		InsecureSkipVerify: true, //nolint:gosec
		MinVersion:         tls.VersionTLS12,
		Certificates:       []tls.Certificate{tlsCert},
		VerifyConnection:   wendyRegistryServerVerifier(caPool),
	}
	return func(ctx context.Context) (net.Conn, error) {
		raw, dialErr := dial(ctx)
		if dialErr != nil {
			return nil, dialErr
		}
		tlsConn := tls.Client(raw, tlsCfg)
		if hsErr := tlsConn.HandshakeContext(ctx); hsErr != nil {
			raw.Close()
			return nil, fmt.Errorf("TLS handshake with device registry: %w", hsErr)
		}
		return tlsConn, nil
	}, nil
}

func resolveRegistryForAppleContainer(ctx context.Context, conn *grpcclient.AgentConnection, port int) (registryAddr string, cleanup func(), useMTLS bool, dialErr func() error, err error) {
	port, err = registryHostPortForAgent(ctx, conn, port)
	if err != nil {
		return "", nil, false, nil, err
	}
	if conn.RegistryDialer != nil || conn.IsMTLS {
		registryAddr, appleUseMTLS, cleanup, proxyDialErr, err := resolveRegistryForSwiftAgent(ctx, conn, port)
		if err != nil {
			return "", nil, false, nil, err
		}
		if appleUseMTLS {
			cleanup()
			return "", nil, false, nil, fmt.Errorf("Apple Container builder cannot push directly to an mTLS registry over this connection; use --builder docker")
		}
		if !registryAddrUsesLoopback(registryAddr) {
			cleanup()
			return "", nil, false, nil, fmt.Errorf("Apple Container builder expected loopback registry proxy, got %q", registryAddr)
		}
		return registryAddr, cleanup, false, proxyDialErr, nil
	}

	targetHost := resolveRegistryIP(conn.Host)
	if isLinkLocalIP(targetHost) {
		targetHost = conn.Host
	}
	target := net.JoinHostPort(targetHost, strconv.Itoa(port))
	proxy, proxyErr := startRegistryProxy(ctx, "127.0.0.1:0", target)
	if proxyErr != nil {
		return "", nil, false, nil, fmt.Errorf("starting Apple Container registry proxy: %w", proxyErr)
	}
	return fmt.Sprintf("127.0.0.1:%d", proxy.Port()), proxy.Close, false, proxy.LastDialError, nil
}

// registryUnavailableError is a build/push failure reinterpreted as "the Mac
// agent has no reachable container registry": the registry proxy recorded a
// refused dial, meaning nothing was listening on the device's registry port.
type registryUnavailableError struct {
	host    string
	dialErr error
}

func (e *registryUnavailableError) Error() string {
	return fmt.Sprintf("the Mac agent at %s isn't running a container registry (%v); "+
		"install Docker, OrbStack, or Apple's `container` CLI on the Mac — or update the "+
		"Wendy agent there if a container runtime is already installed — to run Linux/WendyOS "+
		"apps on it, or set \"platform\": \"darwin\" in wendy.json to run this app natively on "+
		"the Mac instead", e.host, e.dialErr)
}

func (e *registryUnavailableError) Unwrap() error { return e.dialErr }

func isRegistryUnavailable(err error) bool {
	var r *registryUnavailableError
	return errors.As(err, &r)
}

// maybeRegistryUnavailable reinterprets a build/push failure as "no registry on
// the Mac agent" when the registry proxy recorded a refused dial. Darwin-only:
// a Mac agent only runs a container registry when it found a Linux container
// backend at startup (or is too old to serve the LAN listener at all), whereas
// a WendyOS/Linux device always ships its container runtime as part of the OS —
// a refused connection there is an actual fault, not a missing optional
// dependency.
func maybeRegistryUnavailable(agentOS, host string, dialErr func() error, buildErr error) error {
	if !strings.EqualFold(agentOS, appconfig.PlatformDarwin) || dialErr == nil {
		return buildErr
	}
	if de := dialErr(); isDialRefused(de) {
		return &registryUnavailableError{host: host, dialErr: de}
	}
	return buildErr
}

// isDialRefused reports whether err looks like a TCP dial that was actively
// refused — i.e. nothing was listening on the target port at all — as opposed
// to a timeout, reset, or other network failure. Used to recognize "no
// container runtime running on the Mac agent" specifically, so the CLI can
// explain that instead of surfacing a raw dial/registry error.
func isDialRefused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return strings.Contains(err.Error(), "connection refused")
}

// mtlsRegistryHTTPProxy is a plain-HTTP reverse proxy that forwards requests
// to a provisioned device's HTTPS registry using mTLS. The Swift container
// plugin connects to 127.0.0.1:PORT via plain HTTP (with --allow-insecure-http)
// and this proxy handles TLS + client-cert authentication transparently.
type mtlsRegistryHTTPProxy struct {
	listener net.Listener
	server   *http.Server

	mu      sync.Mutex
	dialErr error
}

func (p *mtlsRegistryHTTPProxy) Port() int {
	return p.listener.Addr().(*net.TCPAddr).Port
}

func (p *mtlsRegistryHTTPProxy) Close() {
	_ = p.server.Close()
}

func (p *mtlsRegistryHTTPProxy) recordDialErr(err error) {
	p.mu.Lock()
	p.dialErr = err
	p.mu.Unlock()
}

// LastDialError returns the most recent error encountered while reaching the
// proxy's target (e.g. connection refused because the Mac agent has no
// container runtime listening), or nil if every request so far has reached it.
func (p *mtlsRegistryHTTPProxy) LastDialError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dialErr
}

func startMTLSRegistryHTTPProxy(target, certPEM, keyPEM, caPEM string) (*mtlsRegistryHTTPProxy, error) {
	leafPEM, err := certs.LeafCertificatePEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("extracting leaf cert: %w", err)
	}
	cert, err := tls.X509KeyPair([]byte(leafPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("parsing mTLS certificate: %w", err)
	}
	caPool := x509.NewCertPool()
	if certs.AppendChainToPool(caPool, caPEM) == 0 {
		return nil, fmt.Errorf("no valid CA certificates found in caPEM")
	}

	p := &mtlsRegistryHTTPProxy{}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "https"
			req.URL.Host = target
			req.Host = target
		},
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				// Skip hostname verification: device registry certs are signed by
				// the Wendy CA but may not include the mDNS hostname as a SAN.
				// VerifyConnection performs full chain validation against the Wendy
				// CA instead.
				InsecureSkipVerify: true, //nolint:gosec
				MinVersion:         tls.VersionTLS12,
				VerifyConnection:   wendyRegistryServerVerifier(caPool),
			},
		},
		// The default ErrorHandler logs "http: proxy error: dial tcp ...: connect:
		// connection refused" straight to stderr for every failed request, which
		// reads like a CLI crash rather than "the agent has no container runtime
		// running". Record the error instead so the caller can surface one clear,
		// actionable message once the build actually fails, and silence the noisy
		// default log line (still returns 502 to the pushing client, same as
		// before — the caller establishes trust and gets attribution independent
		// of the request that failed).
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, handlerErr error) {
			p.recordDialErr(handlerErr)
			w.WriteHeader(http.StatusBadGateway)
		},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p.listener = ln
	p.server = &http.Server{Handler: rp}
	go func() { _ = p.server.Serve(ln) }()
	return p, nil
}

// startMTLSRegistryProxy starts a local plain-TCP listener that tunnels each
// accepted connection to target over mTLS using the CLI's client certificate.
// This lets tools that cannot perform mTLS (e.g. swift-container-plugin) push
// to provisioned devices through a localhost address with plain HTTP.
func startMTLSRegistryProxy(ctx context.Context, target string) (*registryProxy, error) {
	certInfo := loadCLICert()
	if certInfo == nil {
		return nil, fmt.Errorf("no CLI certificates available")
	}
	leafPEM, err := certs.LeafCertificatePEM(certInfo.PemCertificate)
	if err != nil {
		return nil, fmt.Errorf("extracting leaf certificate: %w", err)
	}
	keyPEM, err := certInfo.PrivateKeyPEM()
	if err != nil {
		return nil, fmt.Errorf("loading client key: %w", err)
	}
	tlsCert, err := tls.X509KeyPair([]byte(leafPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("loading client certificate: %w", err)
	}
	caPool := x509.NewCertPool()
	if certs.AppendChainToPool(caPool, certInfo.PemCertificateChain) == 0 {
		return nil, fmt.Errorf("no valid CA certificates found in certInfo.PemCertificateChain")
	}
	tlsCfg := &tls.Config{
		// Skip hostname verification; full chain validation happens in VerifyConnection.
		InsecureSkipVerify: true, //nolint:gosec
		MinVersion:         tls.VersionTLS12,
		Certificates:       []tls.Certificate{tlsCert},
		VerifyConnection:   wendyRegistryServerVerifier(caPool),
	}
	dialer := &tls.Dialer{Config: tlsCfg}
	return startRegistryProxyWithDialer(ctx, "127.0.0.1:0", func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", target)
	}, target)
}

// isLinkLocalIP reports whether the given IP string (possibly bracketed) is a
// link-local unicast address (fe80::/10 for IPv6, 169.254.0.0/16 for IPv4).
func isLinkLocalIP(ip string) bool {
	ip = strings.TrimPrefix(ip, "[")
	if idx := strings.Index(ip, "]"); idx >= 0 {
		ip = ip[:idx]
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	return addr.IsLinkLocalUnicast()
}

// registryProxyListenAddr is the address the host-side registry proxy binds to.
// It is always loopback: on Linux buildkitd uses host networking and reaches
// 127.0.0.1 directly; on macOS/Windows the Docker VM forwards
// host.docker.internal to the host's loopback. Binding loopback rather than
// 0.0.0.0 keeps the device registry tunnel off every other interface for the
// duration of a build (WDY-1168).
const registryProxyListenAddr = "127.0.0.1:0"

// registryProxy forwards TCP connections from a local port to a remote device
// registry. This bridges the gap between Docker Desktop's VM (which cannot
// route to link-local addresses) and the host machine (which can).
type registryProxy struct {
	listener net.Listener
	target   string
	dial     func(context.Context) (net.Conn, error)
	cancel   context.CancelFunc
	done     chan struct{}

	mu      sync.Mutex
	dialErr error
}

func startRegistryProxy(ctx context.Context, listenAddr string, target string) (*registryProxy, error) {
	return startRegistryProxyWithDialer(ctx, listenAddr, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target)
	}, target)
}

func startRegistryProxyWithDialer(ctx context.Context, listenAddr string, dial func(context.Context) (net.Conn, error), target ...string) (*registryProxy, error) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, err
	}

	proxyCtx, cancel := context.WithCancel(ctx)
	p := &registryProxy{
		listener: ln,
		dial:     dial,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	if len(target) > 0 {
		p.target = target[0]
	}

	go p.serve(proxyCtx)
	return p, nil
}

func (p *registryProxy) Port() int {
	return p.listener.Addr().(*net.TCPAddr).Port
}

// Close stops the proxy and waits for the serve loop to exit.
func (p *registryProxy) Close() {
	p.cancel()
	p.listener.Close()
	<-p.done
}

func (p *registryProxy) recordDialErr(err error) {
	p.mu.Lock()
	p.dialErr = err
	p.mu.Unlock()
}

// LastDialError returns the error from the most recent attempt to reach the
// proxy's target, or nil if that attempt succeeded (or none was made yet).
func (p *registryProxy) LastDialError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dialErr
}

func (p *registryProxy) serve(ctx context.Context) {
	defer close(p.done)
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.forward(ctx, conn)
	}
}

func (p *registryProxy) forward(ctx context.Context, client net.Conn) {
	defer client.Close()

	// Test-only fault injection: with probability WENDY_REGISTRY_CHAOS, drop the
	// connection before forwarding to simulate the device-registry tunnel
	// hiccuping under load (connection reset / broken pipe). Used to exercise the
	// build+push retry path (WDY-1690) on demand. Off (0) by default.
	if chaosProxyShouldDrop() {
		return
	}

	remote, err := p.dial(ctx)
	if err != nil {
		p.recordDialErr(err)
		return
	}
	// Clear any earlier failure so LastDialError reflects the latest attempt:
	// a transient refusal during builder bootstrap must not misclassify a later
	// healthy push as "registry unavailable".
	p.recordDialErr(nil)
	defer remote.Close()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, remote); done <- struct{}{} }()
	<-done
}

// chaosProxyShouldDrop reports whether this proxied connection should be dropped
// for fault-injection testing, per the WENDY_REGISTRY_CHAOS probability (0..1).
// Returns false (no chaos) when unset, unparseable, or <= 0.
func chaosProxyShouldDrop() bool {
	v := os.Getenv("WENDY_REGISTRY_CHAOS")
	if v == "" {
		return false
	}
	p, err := strconv.ParseFloat(v, 64)
	if err != nil || p <= 0 {
		return false
	}
	return rand.Float64() < p
}

// splitIPv6RegistryAddr checks if registryAddr is a bracketed IPv6 address
// (e.g. "[fe80::1%en0]:5000") and, if so, returns a hostname alias
// ("wendy-registry:<port>") as the effective address and the bare IPv6 IP
// (zone stripped) for use in /etc/hosts. For non-IPv6 addresses, the input
// is returned unchanged and ipv6IP is empty.
func splitIPv6RegistryAddr(registryAddr string) (effectiveAddr, ipv6IP string) {
	idx := strings.Index(registryAddr, "]:")
	if idx == -1 {
		return registryAddr, ""
	}
	raw := registryAddr[1:idx]
	port := registryAddr[idx+2:]
	if addr, err := netip.ParseAddr(raw); err == nil {
		ipv6IP = addr.WithZone("").String()
	} else {
		ipv6IP = raw
	}
	return "wendy-registry:" + port, ipv6IP
}

// resolveRegistryIP resolves a host string to an IP address suitable for use
// inside a Docker buildkit container. It prefers routable addresses but may
// fall back to a zone-less link-local IPv6 address as a last resort.
//
// It handles three cases:
//  1. Hostname — resolved via DNS, preferring IPv4 over IPv6 link-local.
//  2. IPv6 with zone ID (fe80::…%en0) — detected via netip.ParseAddr and
//     returned with the zone stripped (zones are host-specific and don't
//     exist inside the builder container's network namespace).
//  3. Any other IP — returned as-is.
func resolveRegistryIP(host string) string {
	// netip.ParseAddr handles zone IDs; net.ParseIP does not.
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.WithZone("").String()
	}

	// Not a bare IP — treat as hostname and resolve.
	if net.ParseIP(host) == nil {
		if resolved := resolveHostPreferRoutable(host); resolved != "" {
			return resolved
		}
	}
	return host
}

// resolveHostPreferRoutable resolves a hostname and returns the best address
// for use inside a Docker container. It prefers, in order:
//  1. IPv4 addresses (from DNS)
//  2. Global/ULA IPv6 addresses
//  3. IPv4 discovered via ARP/NDP correlation (when DNS only returns link-local IPv6)
//  4. Link-local IPv6 (stripped of zone ID, as a last resort)
func resolveHostPreferRoutable(hostname string) string {
	addrs, err := net.LookupHost(hostname)
	if err != nil || len(addrs) == 0 {
		// The shipped CGO_ENABLED=0 binary can't resolve ".local" via the OS
		// resolver; fall back to an mDNS browse so a device reached by its
		// ".local" name still resolves for registry use (issue #1155).
		if ip := resolveMDNSHost(context.Background(), hostname); ip != "" {
			addrs = []string{ip}
		} else {
			return ""
		}
	}

	// Scan all addresses before returning — IPv4 may appear after global IPv6
	// in the list (e.g. net.LookupHost on macOS returns AAAA records first).
	var globalIPv6, fallbackLinkLocal string
	for _, a := range addrs {
		addr, parseErr := netip.ParseAddr(a)
		if parseErr != nil {
			continue
		}
		if addr.Is4() {
			return a // IPv4 is always preferred
		}
		if !addr.IsLinkLocalUnicast() {
			if globalIPv6 == "" {
				globalIPv6 = addr.WithZone("").String()
			}
		} else if fallbackLinkLocal == "" {
			fallbackLinkLocal = addr.WithZone("").String()
		}
	}

	if globalIPv6 != "" {
		return globalIPv6
	}

	// DNS returned only link-local IPv6 — this is unroutable from Docker
	// containers (zone IDs are host-specific). As a fallback, try to find
	// the device's IPv4 address by looking up the interface for its IPv6
	// link-local neighbor entry, then selecting an IPv4 neighbor on that
	// same interface. This is common for USB-connected devices where
	// mDNS only advertises an AAAA record but the device also has an
	// IPv4 link-local address (169.254.x.x).
	if fallbackLinkLocal != "" {
		if ipv4 := findIPv4ViaNeighborTable(fallbackLinkLocal); ipv4 != "" {
			return ipv4
		}
	}

	return fallbackLinkLocal // link-local without zone as last resort
}

// findIPv4ViaNeighborTable tries to find the IPv4 address of a device known
// by its IPv6 link-local address. It looks up the network interface from the
// NDP table, then finds any IPv4 neighbor on that same interface. This works
// because USB point-to-point links typically have only one peer.
//
// Note: MAC correlation is not used because USB RNDIS/ECM adapters often
// assign different MACs to the IPv4 and IPv6 virtual interfaces.
// Returns "" if no IPv4 address can be found.
func findIPv4ViaNeighborTable(ipv6LinkLocal string) string {
	// Use a context that is canceled on interrupt signals (e.g., Ctrl+C),
	// while still enforcing a maximum 2-second timeout for the lookup.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(sigCtx, 2*time.Second)
	defer cancel()

	var candidate string
	switch runtime.GOOS {
	case "darwin":
		candidate = findIPv4NeighborDarwin(ctx, ipv6LinkLocal)
	case "linux":
		candidate = findIPv4NeighborLinux(ctx, ipv6LinkLocal)
	default:
		return ""
	}

	if candidate == "" {
		return ""
	}

	addr, err := netip.ParseAddr(candidate)
	if err != nil || !addr.Is4() {
		return ""
	}

	// Only accept IPv4 link-local (169.254.0.0/16) addresses here to reduce
	// the risk of correlating the IPv6 link-local to the wrong peer on
	// multi-peer interfaces (e.g., Wi-Fi/Ethernet).
	linkLocalPrefix := netip.PrefixFrom(netip.AddrFrom4([4]byte{169, 254, 0, 0}), 16)
	if !linkLocalPrefix.Contains(addr) {
		return ""
	}

	return addr.String()
}

// findIPv4NeighborDarwin looks up the IPv4 address for a device on macOS.
// It finds the interface from the NDP table, then returns the first IPv4
// neighbor on that interface that isn't a local address.
func findIPv4NeighborDarwin(ctx context.Context, ipv6LinkLocal string) string {
	// Step 1: Find the interface from the NDP table.
	// ndp -an output: "fe80::1%en6  aa:bb:cc:dd:ee:ff  en6  23h49m  S  R"
	ndpOut, err := neighborExecCommandContext(ctx, "ndp", "-an").Output()
	if err != nil {
		return ""
	}

	var iface string
	for _, line := range strings.Split(string(ndpOut), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		addrField := fields[0]
		if idx := strings.Index(addrField, "%"); idx >= 0 {
			addrField = addrField[:idx]
		}
		if addrField != ipv6LinkLocal {
			continue
		}
		iface = fields[2]
		break
	}
	if iface == "" {
		return ""
	}

	// Build a set of local IPv4 addresses on this interface so we can
	// skip them. The ARP table includes "permanent" entries for the
	// host's own addresses which must not be returned as the device IP.
	localAddrs := make(map[string]bool)
	if netIface, ifErr := net.InterfaceByName(iface); ifErr == nil {
		if addrs, addrErr := netIface.Addrs(); addrErr == nil {
			for _, a := range addrs {
				if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.To4() != nil {
					localAddrs[ipNet.IP.String()] = true
				}
			}
		}
	}

	// Step 2: Find a non-local IPv4 neighbor on the same interface.
	// arp -an -i en6 output: "? (169.254.189.250) at aa:bb:cc:dd:ee:ff on en6 ..."
	arpOut, err := neighborExecCommandContext(ctx, "arp", "-an", "-i", iface).Output()
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(arpOut), "\n") {
		start := strings.Index(line, "(")
		end := strings.Index(line, ")")
		if start >= 0 && end > start {
			ip := line[start+1 : end]
			if localAddrs[ip] {
				continue
			}
			if parsed, parseErr := netip.ParseAddr(ip); parseErr == nil && parsed.Is4() {
				return ip
			}
		}
	}

	return ""
}

// findIPv4NeighborLinux looks up the IPv4 address for a device on Linux.
// It finds the interface from the IPv6 neighbor table, then returns the
// first non-local IPv4 neighbor on that interface.
func findIPv4NeighborLinux(ctx context.Context, ipv6LinkLocal string) string {
	// Step 1: Find the interface from ip -6 neigh.
	// Output: "fe80::1 dev eth0 lladdr aa:bb:cc:dd:ee:ff STALE"
	// Parse the target IPv6 address once and strip any zone.
	targetAddr, targetErr := netip.ParseAddr(ipv6LinkLocal)
	if targetErr == nil {
		targetAddr = targetAddr.WithZone("")
	}

	neighOut, err := exec.CommandContext(ctx, "ip", "-6", "neigh", "show").Output()
	if err != nil {
		return ""
	}

	var iface string
	for _, line := range strings.Split(string(neighOut), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		// The first field should be the IPv6 neighbor address, possibly with a zone (e.g., "%eth0").
		addrStr := fields[0]
		if zoneIdx := strings.Index(addrStr, "%"); zoneIdx >= 0 {
			addrStr = addrStr[:zoneIdx]
		}

		parsedAddr, parseErr := netip.ParseAddr(addrStr)
		if parseErr != nil || !parsedAddr.Is6() || targetErr != nil {
			continue
		}
		parsedAddr = parsedAddr.WithZone("")
		if parsedAddr != targetAddr {
			continue
		}

		for i, f := range fields {
			if f == "dev" && i+1 < len(fields) {
				iface = fields[i+1]
				break
			}
		}
		if iface != "" {
			break
		}
	}
	if iface == "" {
		return ""
	}

	// Build a set of local IPv4 addresses on this interface.
	localAddrs := make(map[string]bool)
	if netIface, ifErr := net.InterfaceByName(iface); ifErr == nil {
		if addrs, addrErr := netIface.Addrs(); addrErr == nil {
			for _, a := range addrs {
				if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.To4() != nil {
					localAddrs[ipNet.IP.String()] = true
				}
			}
		}
	}

	// Step 2: Find a non-local IPv4 neighbor on the same interface.
	// Output: "169.254.189.250 dev usb0 lladdr aa:bb:cc:dd:ee:ff REACHABLE"
	arpOut, err := exec.CommandContext(ctx, "ip", "-4", "neigh", "show", "dev", iface).Output()
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(arpOut), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			ip := fields[0]
			if localAddrs[ip] {
				continue
			}
			if parsed, parseErr := netip.ParseAddr(ip); parseErr == nil && parsed.Is4() {
				return ip
			}
		}
	}

	return ""
}

// swiftBuildConfig maps the --debug flag onto a SwiftPM build configuration.
func swiftBuildConfig(debug bool) string {
	if debug {
		return "debug"
	}
	return "release"
}

// buildSwiftDockerImage cross-compiles a Swift package for Linux and builds a
// Docker image containing the resulting binary. Returns the Docker image name.
// Used for Swift projects that do not have a Dockerfile (Docker provider,
// local build path, and provider-build path). buildConfig is the SwiftPM
// configuration to compile with — use swiftBuildConfig to derive it from --debug.
func buildSwiftDockerImage(ctx context.Context, dir, product, arch, buildConfig string, toolchainStdout, toolchainStderr io.Writer) (string, error) {
	sdk, err := swifttoolchain.FindSwiftSDK(ctx, arch, toolchainStdout, toolchainStderr)
	if err != nil {
		return "", fmt.Errorf("finding Swift SDK: %w", err)
	}

	cliLogln("Cross-compiling %s for linux/%s (%s)...", product, arch, buildConfig)
	buildCmd := swifttoolchain.SwiftCommandContext(ctx,
		"build", "-c", buildConfig, "--swift-sdk="+sdk, "--product", product)
	buildCmd.Dir = dir
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return "", fmt.Errorf("swift build: %w", err)
	}

	// Determine the binary output path.
	showBinCmd := swifttoolchain.SwiftCommandContext(ctx,
		"build", "-c", buildConfig, "--swift-sdk="+sdk, "--show-bin-path")
	showBinCmd.Dir = dir
	out, err := showBinCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("swift build --show-bin-path: %w\n%s", err, string(out))
	}
	binDir := strings.TrimSpace(string(out))
	srcBin := filepath.Join(binDir, product)

	// Create a temp directory with the binary and a minimal Dockerfile.
	tmpDir, err := os.MkdirTemp("", "wendy-swift-docker-*")
	if err != nil {
		return "", fmt.Errorf("creating temp directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Copy the cross-compiled binary to a fixed name to avoid Dockerfile
	// issues with special characters in Swift product names.
	dstBin := filepath.Join(tmpDir, "app")
	if err := copyBinary(srcBin, dstBin); err != nil {
		return "", fmt.Errorf("copying binary: %w", err)
	}

	// Write a minimal Dockerfile using the fixed binary name.
	dockerfile := fmt.Sprintf("FROM swift:%s-slim\nCOPY app /usr/local/bin/app\nCMD [\"app\"]\n",
		swifttoolchain.DefaultVersion)
	if err := os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return "", fmt.Errorf("writing Dockerfile: %w", err)
	}

	// Build the Docker image with a sanitised name.
	imageName := sanitizeDockerImageName(product) + ":latest"
	dockerCmd := exec.CommandContext(ctx, "docker", "build", "-t", imageName, ".")
	dockerCmd.Dir = tmpDir
	dockerCmd.Stdout = os.Stdout
	dockerCmd.Stderr = os.Stderr
	if err := dockerCmd.Run(); err != nil {
		return "", fmt.Errorf("docker build: %w", err)
	}

	return imageName, nil
}

// sanitizeDockerImageName produces a valid Docker image reference component
// from an arbitrary string (e.g. a Swift product name).
func sanitizeDockerImageName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	result := strings.Trim(b.String(), "-.")
	if result == "" {
		return "wendy-app"
	}
	return result
}

// copyBinary copies a file from src to dst with mode 0755.
func copyBinary(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	mode := srcInfo.Mode().Perm() | 0o111

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
