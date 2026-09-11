package solve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/containerd/platforms"
	"github.com/docker/cli/cli/config"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/tonistiigi/fsutil"
	"golang.org/x/sync/errgroup"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/llbgen"

	// The docker-container connection helper registers itself on import, and
	// client.New picks it up by URL scheme. Without this blank import, the
	// address Address returns on macOS dials nothing.
	_ "github.com/moby/buildkit/client/connhelper/dockercontainer"
)

// Output is where a solved build is written. Exactly one destination is set:
// an OCI-layout tar on disk (what the chunk-diff deploy path consumes) or a
// named image in the daemon's store, optionally pushed.
type Output struct {
	// OCILayoutPath, when set, writes the image as an OCI-layout tar there.
	OCILayoutPath string
	// ImageRef names the image. It is required for a push and optional for an
	// OCI layout, where it becomes the name recorded in the layout's index.
	ImageRef string
	// Push publishes ImageRef to its registry.
	Push bool
}

// Request is one build: a compiled definition, the metadata to stamp on the
// image it produces, and where to put the result.
type Request struct {
	// Def is llbgen.Emit's first return value.
	Def *llb.Definition
	// Config is llbgen.Emit's second return value: the final stage's
	// entrypoint and user, which LLB has no way to carry.
	Config *llbgen.ImageConfig
	// BaseConfig is the raw image config of the *final stage's base image* —
	// one entry out of the map handed to Emit, not the whole map. It is the
	// config the produced image inherits from (see imageConfig), and is
	// distinct from Config in both direction and content: Config is what this
	// build adds, BaseConfig is what it starts from.
	BaseConfig []byte
	// Platform is the target, in the same form Emit was given. It labels the
	// produced image; the exporter refuses a config without one.
	Platform string
	// ContextDir is the directory mounted as the build context. It must be the
	// directory the graph's local copies were written against.
	ContextDir string
	Output     Output
	// Progress receives BuildKit's plain progress stream. Callers inside the
	// CLI pass the writer runBuildWithProgress hands them, so an LLB build
	// renders through the same step list as every other build. A nil writer
	// discards it.
	Progress io.Writer
}

// Run solves req against the buildkitd at addr.
//
// The build goes through client.Build with a gateway callback rather than the
// simpler client.Solve, and that is not incidental: a raw Solve exports the
// filesystem the definition describes and nothing else. Entrypoint and user
// live in the image config, which only a gateway result can carry, so a Solve
// here would produce an image that runs the base image's command as root.
func Run(ctx context.Context, addr string, req Request) error {
	if req.Def == nil {
		return fmt.Errorf("no LLB definition to solve")
	}
	if req.ContextDir == "" {
		return fmt.Errorf("no build context directory")
	}

	imgConfig, err := imageConfig(req.BaseConfig, req.Config, req.Platform)
	if err != nil {
		return err
	}
	export, err := req.Output.exportEntry()
	if err != nil {
		return err
	}
	contextFS, err := fsutil.NewFS(req.ContextDir)
	if err != nil {
		return fmt.Errorf("build context %q: %w", req.ContextDir, err)
	}

	progressOut := req.Progress
	if progressOut == nil {
		progressOut = io.Discard
	}
	// PlainMode unconditionally: this writer is a parser, not a terminal. The
	// live step list is rendered further up by runBuildWithProgress, which
	// consumes exactly this format from buildx today.
	display, err := progressui.NewDisplay(progressOut, progressui.PlainMode)
	if err != nil {
		return fmt.Errorf("build progress: %w", err)
	}

	// This fires only for an address grpc cannot even parse. The dial is lazy,
	// so an absent daemon surfaces from Build's first RPC instead — see
	// solveError.
	c, err := client.New(ctx, addr)
	if err != nil {
		return fmt.Errorf("buildkitd address %s: %w", addr, err)
	}
	defer c.Close()

	opt := client.SolveOpt{
		Exports: []client.ExportEntry{export},
		// The definition's local source is named by llbgen; mounting it under
		// any other name leaves that source unsatisfied.
		LocalMounts: map[string]fsutil.FS{llbgen.LocalContextName: contextFS},
		// Lets buildkitd diff this context against the previous transfer of the
		// same directory instead of re-sending it every build.
		SharedKey: req.ContextDir,
		Session:   []session.Attachable{dockerAuthProvider()},
	}

	statusCh := make(chan *client.SolveStatus)
	eg, egCtx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		// Build closes statusCh itself, which is what ends the display below.
		_, err := c.Build(egCtx, opt, "", func(ctx context.Context, gw gateway.Client) (*gateway.Result, error) {
			// Evaluate forces the definition to run inside this call, so a
			// failing step surfaces here with its vertex attached rather than
			// as an opaque error from the exporter afterwards.
			res, err := gw.Solve(ctx, gateway.SolveRequest{Definition: req.Def.ToPB(), Evaluate: true})
			if err != nil {
				return nil, err
			}
			ref, err := res.SingleRef()
			if err != nil {
				return nil, err
			}
			out := gateway.NewResult()
			out.SetRef(ref)
			out.AddMeta(exptypes.ExporterImageConfigKey, imgConfig)
			return out, nil
		}, statusCh)
		return err
	})

	eg.Go(func() error {
		// Deliberately not egCtx: when the build fails, errgroup cancels its
		// context immediately, and a display that stopped on that cancellation
		// would swallow the last status messages — including the ones naming
		// the step that failed.
		_, err := display.UpdateFrom(context.WithoutCancel(ctx), statusCh)
		return err
	})

	if err := eg.Wait(); err != nil {
		// A cancelled build is the user's own Ctrl-C; dressing it up as a
		// daemon problem would be a lie, and callers test for it.
		if ctx.Err() != nil {
			return err
		}
		return solveError(addr, err)
	}
	return nil
}

// solveError gives a failed build the one piece of context BuildKit's own error
// cannot carry: which daemon it was talking to.
//
// The connection failure for a daemon that is not running does not come from
// client.New — that dial is lazy — it comes from the first RPC inside Build, as
// "listing workers for Build: rpc error ... No such container:
// buildx_buildkit_wendy0". That names a container the user never created and
// gives no hint that a builder has to exist first, so the wrapper says where it
// was pointed and how to point it elsewhere. Build failures pass through the
// same wrapper; naming the daemon is harmless there and the cause is preserved
// either way.
func solveError(addr string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("build against buildkitd at %s: %w (if the daemon is unreachable, its buildx builder may not have been created yet, or set BUILDKIT_HOST to a running daemon)", addr, err)
}

// FinalBaseConfig picks the entry Request.BaseConfig wants out of the map that
// was handed to llbgen.Emit: the config of the base image the final stage is
// built on, which is the config the produced image inherits.
//
// It exists so a caller does not re-derive "which of these images is the one
// that matters" by hand. Picking a different stage's base image is not a
// failure any test would catch — it produces an image whose environment and
// working directory come from a builder stage that was thrown away.
func FinalBaseConfig(g *ir.Graph, configs map[string][]byte) ([]byte, error) {
	if g == nil || len(g.Stages) == 0 {
		return nil, fmt.Errorf("graph has no stages")
	}
	final := g.Stages[len(g.Stages)-1]
	start := 0
	if len(g.Stages) > 1 {
		start = g.Stages[len(g.Stages)-2].Final + 1
	}
	if final.Final < start || final.Final >= len(g.Nodes) {
		return nil, fmt.Errorf("stage %q: final node %d is outside the graph's %d nodes", final.Name, final.Final, len(g.Nodes))
	}

	for i := start; i <= final.Final; i++ {
		n := g.Nodes[i]
		if n.Kind != ir.OpImage {
			continue
		}
		if n.Image == nil {
			return nil, fmt.Errorf("stage %q: node %d has kind %q but nil Image payload", final.Name, i, n.Kind)
		}
		cfg, ok := configs[n.Image.Ref]
		if !ok {
			return nil, fmt.Errorf("no resolved image config for %q; resolve it alongside the digest", n.Image.Ref)
		}
		return cfg, nil
	}
	return nil, fmt.Errorf("stage %q has no base image", final.Name)
}

// dockerAuthProvider forwards the host's registry credentials to buildkitd over
// the build session. Without it every pull and push is anonymous: base images
// from a private registry fail to resolve, and a push to Wendy's per-org
// registry is rejected.
func dockerAuthProvider() session.Attachable {
	return authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{
		// A corrupt ~/.docker/config.json makes LoadDefaultConfigFile warn and
		// continue with an empty config, so every pull silently becomes
		// anonymous — the exact failure this provider exists to prevent, made
		// invisible. The warning goes to stderr, as buildx sends it: it is
		// written once here, before the progress display starts, so it does not
		// interleave with the step list.
		AuthConfigProvider: authprovider.LoadAuthConfig(config.LoadDefaultConfigFile(os.Stderr)),
	})
}

// exportEntry converts o into the single exporter a solve is configured with.
func (o Output) exportEntry() (client.ExportEntry, error) {
	switch {
	case o.OCILayoutPath != "" && o.Push:
		return client.ExportEntry{}, fmt.Errorf("an OCI layout export cannot also push; choose one destination")
	case o.OCILayoutPath != "":
		attrs := map[string]string{}
		if o.ImageRef != "" {
			attrs["name"] = o.ImageRef
		}
		path := o.OCILayoutPath
		return client.ExportEntry{
			Type:  client.ExporterOCI,
			Attrs: attrs,
			// Called once the build succeeds, so a failed build leaves no
			// truncated tar behind for a later step to mistake for output.
			Output: func(map[string]string) (io.WriteCloser, error) { return os.Create(path) },
		}, nil
	case o.ImageRef != "":
		attrs := map[string]string{"name": o.ImageRef}
		if o.Push {
			attrs["push"] = "true"
		}
		return client.ExportEntry{Type: client.ExporterImage, Attrs: attrs}, nil
	case o.Push:
		return client.ExportEntry{}, fmt.Errorf("push needs an image reference to push to")
	default:
		return client.ExportEntry{}, fmt.Errorf("no output configured: set an OCI layout path or an image reference")
	}
}

// imageConfig builds the config stamped onto the produced image: the final
// stage's base image config with this build's entrypoint and user written over
// it, relabelled for the target platform.
//
// Inheriting rather than starting fresh is the whole point. `FROM python:3.12`
// followed by ENTRYPOINT yields an image that still carries python's PATH, its
// WORKDIR, its exposed ports and its labels, because the builder resolves the
// base config and edits it. An image config built from nothing would run the
// same filesystem with an empty environment — a build that succeeds and
// produces a container that cannot find its own interpreter.
//
// The base config is edited as raw JSON rather than round-tripped through a Go
// struct so that fields no struct here models — Healthcheck, Shell, OnBuild,
// and whatever a future spec adds — survive instead of being silently dropped.
// The cost of that fidelity is droppedBaseFields: raw JSON also preserves
// fields the Dockerfile path drops, and preserving those is just as much a
// divergence as losing the others.
func imageConfig(base []byte, cfg *llbgen.ImageConfig, platform string) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("no image config for the final stage")
	}
	if len(base) == 0 {
		// llbgen.Emit refuses a missing base config for the same reason; a
		// caller that reached here without one has skipped resolution, not
		// asked for a bare image.
		return nil, fmt.Errorf("no base image config; resolve it alongside the digest")
	}

	p, err := platforms.Parse(platform)
	if err != nil {
		return nil, fmt.Errorf("platform %q: %w", platform, err)
	}
	p = platforms.Normalize(p)

	var img map[string]json.RawMessage
	if err := json.Unmarshal(base, &img); err != nil {
		return nil, fmt.Errorf("base image config is not valid JSON: %w", err)
	}
	if img == nil {
		return nil, fmt.Errorf("base image config is null")
	}

	for _, key := range droppedBaseFields {
		delete(img, key)
	}

	inner := map[string]json.RawMessage{}
	if raw, ok := img["config"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &inner); err != nil {
			return nil, fmt.Errorf("base image config's config object is not valid JSON: %w", err)
		}
	}

	for _, key := range droppedBaseConfigFields {
		delete(inner, key)
	}

	// Env is merged onto the base image's rather than replacing it, which is
	// what a Dockerfile ENV does: a stage that sets one variable keeps the
	// base image's PATH. A declared key overrides the base's entry in place,
	// so the variable keeps its original position and a later entry cannot
	// shadow it.
	if len(cfg.Env) > 0 {
		env, err := mergeEnv(inner["Env"], cfg.Env)
		if err != nil {
			return nil, err
		}
		if err := setJSON(inner, "Env", env); err != nil {
			return nil, err
		}
	}

	if cfg.Workdir != "" {
		if err := setJSON(inner, "WorkingDir", cfg.Workdir); err != nil {
			return nil, err
		}
	}

	if cfg.Entrypoint != nil {
		ep, err := json.Marshal(cfg.Entrypoint)
		if err != nil {
			return nil, err
		}
		inner["Entrypoint"] = ep
		// Dockerfile's ENTRYPOINT resets CMD. Leaving the base image's Cmd in
		// place would append its arguments to the entrypoint at run time, so
		// the two backends would start different processes from one Stagefile.
		delete(inner, "Cmd")
	}

	// Written after the reset above, so a stage declaring both keeps its own
	// cmd — the order a Dockerfile's ENTRYPOINT-then-CMD produces.
	if len(cfg.Cmd) > 0 {
		if err := setJSON(inner, "Cmd", cfg.Cmd); err != nil {
			return nil, err
		}
	}

	if cfg.Healthcheck != nil {
		hc, err := healthcheckConfig(cfg.Healthcheck)
		if err != nil {
			return nil, err
		}
		if err := setJSON(inner, "Healthcheck", hc); err != nil {
			return nil, err
		}
	}

	// An empty user is root. ir.DefaultUser is the value every backend
	// substitutes for a stage that declares none; Emit already applies it, and
	// applying it again costs nothing and closes the hand-built-config gap.
	user := cfg.User
	if user == "" {
		user = ir.DefaultUser
	}
	u, err := json.Marshal(user)
	if err != nil {
		return nil, err
	}
	inner["User"] = u

	cfgJSON, err := json.Marshal(inner)
	if err != nil {
		return nil, err
	}
	img["config"] = cfgJSON

	// The platform is the build's, not the base image's. They agree today —
	// Emit rejects a base config that disagrees — but the exporter reads os and
	// architecture from this config to label the manifest, so writing them
	// explicitly keeps a mislabelled base from mislabelling the output.
	if err := setJSON(img, "os", p.OS); err != nil {
		return nil, err
	}
	if err := setJSON(img, "architecture", p.Architecture); err != nil {
		return nil, err
	}
	// A variant left over from the base would contradict the architecture just
	// written — "arm64" carrying arm's "v7".
	if p.Variant == "" {
		delete(img, "variant")
	} else if err := setJSON(img, "variant", p.Variant); err != nil {
		return nil, err
	}

	return json.Marshal(img)
}

// droppedBaseFields are the top-level base-config keys that must not reach the
// produced image, because the Dockerfile backend does not carry them either.
//
// dockerfile2llb unmarshals the base config into dockerspec.DockerOCIImage,
// which has no field for "container", "container_config", "docker_version" or
// "moby.buildkit.cache.v0", so they vanish on the round trip. Preserving them
// here would give one Stagefile two different config digests — and therefore
// two different image IDs — depending only on whether its base image was built
// by the classic docker builder or carries inline cache. Nothing in the
// Stagefile would explain the difference.
//
// "created" is dropped for a different reason: the image exporter backfills it
// with the build time, but only when the key is absent (patchImageConfig treats
// a present "created" as authoritative and merely clamps it). Inheriting the
// base's value would ship an image dated to whenever Debian last published,
// which `docker images` reports as months old and which changes the config
// digest against a Dockerfile build of the same source.
var droppedBaseFields = []string{
	"created",
	"container",
	"container_config",
	"docker_version",
	"moby.buildkit.cache.v0",
}

// droppedBaseConfigFields is droppedBaseFields one level down: the keys inside
// the base config's "config" object that must not reach the produced image.
//
// They are the Docker v1 container-creation defaults — the ones dockerd used to
// stamp into an image config alongside the fields that describe the image
// itself. dockerspec.DockerOCIImageConfig is ocispecs.ImageConfig plus exactly
// Healthcheck, OnBuild and Shell, so it models none of these and
// dockerfile2llb's unmarshal/marshal round trip drops all of them. No OCI
// runtime reads any of them from an image config either: they are `docker run`
// defaults, and "Image" is just the config digest of whatever the classic
// builder committed from.
//
// This list was found by the differential test rather than reasoned out in
// advance. Every Examples project on a BuildKit-built base agreed; the one on a
// base built by the classic builder (dustynv/pytorch, docker_version 24.0.7)
// diverged on all nine at once, with identical rootfs diff IDs — the same
// Stagefile, the same filesystem, two different image IDs, and nothing in the
// Stagefile to explain which one you got. That is precisely the failure
// droppedBaseFields exists to prevent; it was only ever half-applied.
var droppedBaseConfigFields = []string{
	"AttachStderr",
	"AttachStdin",
	"AttachStdout",
	"Domainname",
	"Hostname",
	"Image",
	"OpenStdin",
	"StdinOnce",
	"Tty",
}

// mergeEnv applies the stage's declared variables to the base image's Env
// list, the way a Dockerfile ENV does: an existing key is replaced where it
// already sits, and a new one is appended.
//
// Replacing in place rather than appending matters because an image config's
// Env is an ordered list that may legally repeat a key, and a runtime takes
// the last occurrence. Appending would leave the base's value behind the new
// one, which works — until something reads the list rather than resolving it.
// Declared keys are applied in sorted order, matching the order codegen emits
// ENV lines in, so a value referring to another variable resolves alike under
// both backends.
func mergeEnv(baseRaw json.RawMessage, declared map[string]string) ([]string, error) {
	var env []string
	if len(baseRaw) > 0 && string(baseRaw) != "null" {
		if err := json.Unmarshal(baseRaw, &env); err != nil {
			return nil, fmt.Errorf("base image config's Env is not a string list: %w", err)
		}
	}

	keys := make([]string, 0, len(declared))
	for k := range declared {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		entry := k + "=" + declared[k]
		replaced := false
		for i, e := range env {
			name, _, _ := strings.Cut(e, "=")
			if name == k {
				env[i] = entry
				replaced = true
				break
			}
		}
		if !replaced {
			env = append(env, entry)
		}
	}
	return env, nil
}

// dockerHealthcheck is the image config's Healthcheck object. Durations are
// nanoseconds there, while a Stagefile writes them as Go-style strings, so the
// conversion happens here rather than anywhere a string could leak through.
type dockerHealthcheck struct {
	Test        []string `json:"Test,omitempty"`
	Interval    int64    `json:"Interval,omitempty"`
	Timeout     int64    `json:"Timeout,omitempty"`
	StartPeriod int64    `json:"StartPeriod,omitempty"`
	Retries     int      `json:"Retries,omitempty"`
}

// healthcheckConfig converts the stage's healthcheck into the image config's
// form. The "CMD" prefix is what marks the exec form; without it a runtime
// reads the first element as the probe's shell string.
func healthcheckConfig(h *ir.Healthcheck) (*dockerHealthcheck, error) {
	if len(h.Exec) == 0 {
		return nil, fmt.Errorf("healthcheck has no command")
	}
	out := &dockerHealthcheck{
		Test:    append([]string{"CMD"}, h.Exec...),
		Retries: h.Retries,
	}
	for _, f := range []struct {
		name string
		src  string
		dst  *int64
	}{
		{"interval", h.Interval, &out.Interval},
		{"timeout", h.Timeout, &out.Timeout},
		{"startPeriod", h.StartPeriod, &out.StartPeriod},
	} {
		if f.src == "" {
			continue
		}
		d, err := time.ParseDuration(f.src)
		if err != nil {
			return nil, fmt.Errorf("healthcheck %s %q: %w", f.name, f.src, err)
		}
		*f.dst = int64(d)
	}
	return out, nil
}

func setJSON(m map[string]json.RawMessage, key string, v any) error {
	dt, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m[key] = dt
	return nil
}
