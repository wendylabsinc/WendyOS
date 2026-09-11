package solve

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/moby/buildkit/client"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/llbgen"
)

// baseConfig is a plausible registry-resolved image config: the environment and
// working directory a Dockerfile build inherits, a Cmd the base declares, and a
// Docker-only extension field no OCI struct in this repo models.
const baseConfig = `{
  "architecture": "arm64",
  "os": "linux",
  "author": "Debian",
  "config": {
    "Env": ["PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8"],
    "WorkingDir": "/app",
    "Cmd": ["python3"],
    "ExposedPorts": {"8080/tcp": {}},
    "Healthcheck": {"Test": ["CMD", "true"]}
  }
}`

func decode(t *testing.T, dt []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(dt, &m); err != nil {
		t.Fatalf("unmarshal produced config: %v", err)
	}
	return m
}

func innerConfig(t *testing.T, dt []byte) map[string]any {
	t.Helper()
	m := decode(t, dt)
	inner, ok := m["config"].(map[string]any)
	if !ok {
		t.Fatalf("produced config has no config object: %s", dt)
	}
	return inner
}

// The output config is the base config with the final stage's metadata written
// over it — not a fresh one. A fresh config would drop PATH and WorkingDir, and
// the image would run differently from the one the Dockerfile backend builds
// out of the same Stagefile.
func TestImageConfigInheritsTheBaseConfig(t *testing.T) {
	dt, err := imageConfig([]byte(baseConfig), &llbgen.ImageConfig{Entrypoint: []string{"/app/server"}, User: "1000"}, "linux/arm64")
	if err != nil {
		t.Fatalf("imageConfig: %v", err)
	}
	inner := innerConfig(t, dt)

	env, _ := json.Marshal(inner["Env"])
	if !strings.Contains(string(env), "PATH=/usr/local/bin") || !strings.Contains(string(env), "LANG=C.UTF-8") {
		t.Fatalf("Env = %s, want the base image's environment", env)
	}
	if inner["WorkingDir"] != "/app" {
		t.Fatalf("WorkingDir = %v, want /app", inner["WorkingDir"])
	}
	if _, ok := inner["ExposedPorts"]; !ok {
		t.Fatalf("ExposedPorts was dropped: %s", dt)
	}
	// Fields no struct in this repo models must survive the round trip too.
	if _, ok := inner["Healthcheck"]; !ok {
		t.Fatalf("Healthcheck was dropped: %s", dt)
	}
	if decode(t, dt)["author"] != "Debian" {
		t.Fatalf("top-level base fields were dropped: %s", dt)
	}
}

func TestImageConfigStampsEntrypointAndUser(t *testing.T) {
	dt, err := imageConfig([]byte(baseConfig), &llbgen.ImageConfig{Entrypoint: []string{"/app/server", "--port=8080"}, User: "1000"}, "linux/arm64")
	if err != nil {
		t.Fatalf("imageConfig: %v", err)
	}
	inner := innerConfig(t, dt)

	ep, _ := json.Marshal(inner["Entrypoint"])
	if string(ep) != `["/app/server","--port=8080"]` {
		t.Fatalf("Entrypoint = %s", ep)
	}
	if inner["User"] != "1000" {
		t.Fatalf("User = %v, want 1000", inner["User"])
	}
}

// Dockerfile's ENTRYPOINT resets CMD. Leaving the base image's Cmd in place
// would append its arguments to our entrypoint — `python3` handed to the app
// binary as an argument.
func TestImageConfigResetsCmdWhenAnEntrypointIsSet(t *testing.T) {
	dt, err := imageConfig([]byte(baseConfig), &llbgen.ImageConfig{Entrypoint: []string{"/app/server"}, User: "1000"}, "linux/arm64")
	if err != nil {
		t.Fatalf("imageConfig: %v", err)
	}
	if cmd, ok := innerConfig(t, dt)["Cmd"]; ok && cmd != nil {
		t.Fatalf("Cmd = %v, want it cleared by the entrypoint", cmd)
	}
}

// codegen emits no ENTRYPOINT line when the stage declares none, so the base
// image's own Cmd is what runs. This backend must agree.
func TestImageConfigKeepsCmdWithoutAnEntrypoint(t *testing.T) {
	dt, err := imageConfig([]byte(baseConfig), &llbgen.ImageConfig{User: "1000"}, "linux/arm64")
	if err != nil {
		t.Fatalf("imageConfig: %v", err)
	}
	inner := innerConfig(t, dt)
	cmd, _ := json.Marshal(inner["Cmd"])
	if string(cmd) != `["python3"]` {
		t.Fatalf("Cmd = %s, want the base image's", cmd)
	}
	if _, ok := inner["Entrypoint"]; ok {
		t.Fatalf("Entrypoint was invented: %s", dt)
	}
}

// An empty user is root. Both backends substitute ir.DefaultUser for a stage
// that declares none; a config that shipped "" would quietly run as root.
func TestImageConfigNeverLeavesTheUserEmpty(t *testing.T) {
	dt, err := imageConfig([]byte(baseConfig), &llbgen.ImageConfig{}, "linux/arm64")
	if err != nil {
		t.Fatalf("imageConfig: %v", err)
	}
	if got := innerConfig(t, dt)["User"]; got != ir.DefaultUser {
		t.Fatalf("User = %v, want %s", got, ir.DefaultUser)
	}
}

// The image exporter refuses a config without os and architecture, and a config
// carrying the base's platform rather than the build's would mislabel a
// cross-built image.
func TestImageConfigStampsTheTargetPlatform(t *testing.T) {
	dt, err := imageConfig([]byte(`{"architecture":"amd64","os":"linux","variant":"stale"}`), &llbgen.ImageConfig{User: "1000"}, "linux/arm/v7")
	if err != nil {
		t.Fatalf("imageConfig: %v", err)
	}
	m := decode(t, dt)
	if m["os"] != "linux" || m["architecture"] != "arm" || m["variant"] != "v7" {
		t.Fatalf("platform fields = %v/%v/%v", m["os"], m["architecture"], m["variant"])
	}
}

func TestImageConfigDropsAStaleVariant(t *testing.T) {
	dt, err := imageConfig([]byte(`{"architecture":"arm","os":"linux","variant":"v7"}`), &llbgen.ImageConfig{User: "1000"}, "linux/arm64")
	if err != nil {
		t.Fatalf("imageConfig: %v", err)
	}
	if v, ok := decode(t, dt)["variant"]; ok {
		t.Fatalf("variant = %v, want it dropped for arm64", v)
	}
}

// A missing base config is a caller that never resolved one. Defaulting to an
// empty config is exactly the silent PATH/WorkingDir loss llbgen refuses at
// Emit time; refusing it again here keeps the two ends consistent.
func TestImageConfigRequiresABaseConfig(t *testing.T) {
	for _, base := range [][]byte{nil, []byte(""), []byte("null")} {
		if _, err := imageConfig(base, &llbgen.ImageConfig{User: "1000"}, "linux/arm64"); err == nil {
			t.Fatalf("expected an error for base config %q", base)
		}
	}
}

func TestImageConfigRejectsAnUnparseablePlatform(t *testing.T) {
	if _, err := imageConfig([]byte(baseConfig), &llbgen.ImageConfig{User: "1000"}, ""); err == nil {
		t.Fatal("expected an error for an empty platform")
	}
}

// A base image built by the classic docker builder carries fields the
// Dockerfile path silently drops, because dockerfile2llb round-trips the config
// through dockerspec.DockerOCIImage, which has no field for them. Keeping them
// gives the same Stagefile a different config digest depending on which backend
// built it — a difference with no cause visible anywhere in the Stagefile.
func TestImageConfigDropsFieldsTheDockerfilePathDrops(t *testing.T) {
	base := `{
	  "architecture": "arm64",
	  "os": "linux",
	  "created": "2019-01-01T00:00:00Z",
	  "docker_version": "18.06.1-ce",
	  "container": "8f2e1b3c",
	  "container_config": {"Hostname": "8f2e1b3c"},
	  "moby.buildkit.cache.v0": "eyJsYXllcnMiOltdfQ==",
	  "config": {"Env": ["PATH=/usr/bin"]}
	}`
	dt, err := imageConfig([]byte(base), &llbgen.ImageConfig{User: "1000"}, "linux/arm64")
	if err != nil {
		t.Fatalf("imageConfig: %v", err)
	}
	m := decode(t, dt)
	for _, key := range []string{"created", "docker_version", "container", "container_config", "moby.buildkit.cache.v0"} {
		if _, ok := m[key]; ok {
			t.Fatalf("%q survived into the produced config: %s", key, dt)
		}
	}
	// The drop is a denylist, not a purge: everything the Dockerfile path keeps
	// must still be here.
	if m["os"] != "linux" {
		t.Fatalf("os was dropped: %s", dt)
	}
	if _, ok := innerConfig(t, dt)["Env"]; !ok {
		t.Fatalf("Env was dropped: %s", dt)
	}
}

// The same denylist one level down. A base image built by the classic docker
// builder carries the Docker v1 container-creation defaults inside its config
// object; dockerspec.DockerOCIImageConfig models none of them, so the Dockerfile
// backend drops them and preserving them here gives one Stagefile two image IDs
// depending only on which builder published its base image.
//
// This is not hypothetical: it is the one divergence the differential test found
// across the Examples corpus, on dustynv/pytorch.
func TestImageConfigDropsTheDockerV1ContainerDefaults(t *testing.T) {
	base := `{
	  "architecture": "arm64",
	  "os": "linux",
	  "config": {
	    "Hostname": "",
	    "Domainname": "",
	    "AttachStdin": false,
	    "AttachStdout": false,
	    "AttachStderr": false,
	    "Tty": false,
	    "OpenStdin": false,
	    "StdinOnce": false,
	    "Image": "sha256:8f446dd738f46afcdee87275799997702d3e0dac608be48da8dbe6b5167c8c71",
	    "Env": ["PATH=/usr/bin"],
	    "Shell": ["/bin/bash", "-c"],
	    "OnBuild": null
	  }
	}`
	dt, err := imageConfig([]byte(base), &llbgen.ImageConfig{User: "1000"}, "linux/arm64")
	if err != nil {
		t.Fatalf("imageConfig: %v", err)
	}
	inner := innerConfig(t, dt)
	for _, key := range []string{"Hostname", "Domainname", "AttachStdin", "AttachStdout", "AttachStderr", "Tty", "OpenStdin", "StdinOnce", "Image"} {
		if _, ok := inner[key]; ok {
			t.Errorf("config.%s survived into the produced config: %s", key, dt)
		}
	}
	// Again a denylist, not a purge — and specifically not a purge of the three
	// Docker-only fields dockerspec *does* model, which the Dockerfile backend
	// keeps and which a coarser "drop anything non-OCI" rule would lose.
	for _, key := range []string{"Env", "Shell", "User"} {
		if _, ok := inner[key]; !ok {
			t.Errorf("config.%s was dropped: %s", key, dt)
		}
	}
}

// created in particular is not merely noise: the exporter backfills it with the
// build time only when the key is absent (patchImageConfig in the image
// exporter), so carrying the base's value forward would ship an image
// `docker images` reports as months old.
func TestImageConfigLeavesCreatedForTheExporterToFill(t *testing.T) {
	dt, err := imageConfig([]byte(baseConfig), &llbgen.ImageConfig{User: "1000"}, "linux/arm64")
	if err != nil {
		t.Fatalf("imageConfig: %v", err)
	}
	if _, ok := decode(t, dt)["created"]; ok {
		t.Fatalf("created must be absent so the exporter fills it: %s", dt)
	}
}

// client.New dials lazily, so an absent daemon does not fail there — it fails
// at Build's first RPC, as a raw "No such container: buildx_buildkit_wendy0".
// Unwrapped, that names an implementation detail the user never chose.
func TestSolveErrorNamesTheAddressAndTheWayOut(t *testing.T) {
	err := solveError("docker-container://buildx_buildkit_wendy0", errors.New("listing workers for Build: rpc error: No such container: buildx_buildkit_wendy0"))
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "docker-container://buildx_buildkit_wendy0") {
		t.Fatalf("error should name the resolved address, got %v", err)
	}
	if !strings.Contains(msg, "BUILDKIT_HOST") {
		t.Fatalf("error should point at the override, got %v", err)
	}
	if !strings.Contains(msg, "No such container") {
		t.Fatalf("error should keep the underlying cause, got %v", err)
	}
}

func TestSolveErrorPassesNilThrough(t *testing.T) {
	if err := solveError("unix:///run/buildkit/buildkitd.sock", nil); err != nil {
		t.Fatalf("solveError(nil) = %v, want nil", err)
	}
}

// twoStageGraph is a builder stage on golang followed by a runtime stage on
// debian — the shape whose final base image is not the first one in the graph.
func twoStageGraph() *ir.Graph {
	return &ir.Graph{
		Nodes: []ir.Node{
			{Kind: ir.OpImage, Image: &ir.ImageOp{Ref: "golang:1.24"}},
			{Kind: ir.OpImage, Image: &ir.ImageOp{Ref: "debian:12"}},
			{Kind: ir.OpCopy, Inputs: []int{1, 0}, Copy: &ir.CopyOp{Paths: []string{"/out/app"}, Dest: "/app"}},
		},
		Stages: []ir.Stage{
			{Name: "builder", Final: 0},
			{Name: "runtime", Final: 2},
		},
	}
}

func TestFinalBaseConfigPicksTheFinalStagesBaseImage(t *testing.T) {
	configs := map[string][]byte{
		"golang:1.24": []byte(`{"os":"builder"}`),
		"debian:12":   []byte(`{"os":"runtime"}`),
	}
	got, err := FinalBaseConfig(twoStageGraph(), configs)
	if err != nil {
		t.Fatalf("FinalBaseConfig: %v", err)
	}
	if string(got) != `{"os":"runtime"}` {
		t.Fatalf("config = %s, want the runtime stage's base", got)
	}
}

func TestFinalBaseConfigRequiresAResolvedConfig(t *testing.T) {
	// The builder stage's config being present is not enough.
	configs := map[string][]byte{"golang:1.24": []byte(`{}`)}
	_, err := FinalBaseConfig(twoStageGraph(), configs)
	if err == nil {
		t.Fatal("expected an error for the missing runtime config")
	}
	if !strings.Contains(err.Error(), "debian:12") {
		t.Fatalf("error should name the unresolved image, got %v", err)
	}
}

func TestFinalBaseConfigRejectsAnEmptyGraph(t *testing.T) {
	if _, err := FinalBaseConfig(&ir.Graph{}, nil); err == nil {
		t.Fatal("expected an error for a graph with no stages")
	}
}

func TestOutputExportEntry(t *testing.T) {
	t.Run("oci layout", func(t *testing.T) {
		e, err := Output{OCILayoutPath: "/tmp/img.tar", ImageRef: "wendy/app:dev"}.exportEntry()
		if err != nil {
			t.Fatalf("exportEntry: %v", err)
		}
		if e.Type != client.ExporterOCI {
			t.Fatalf("Type = %q, want %q", e.Type, client.ExporterOCI)
		}
		if e.Output == nil {
			t.Fatal("an OCI layout export needs a file writer")
		}
		if e.Attrs["name"] != "wendy/app:dev" {
			t.Fatalf("Attrs = %v", e.Attrs)
		}
	})

	t.Run("registry push", func(t *testing.T) {
		e, err := Output{ImageRef: "reg.example/app:dev", Push: true}.exportEntry()
		if err != nil {
			t.Fatalf("exportEntry: %v", err)
		}
		if e.Type != client.ExporterImage {
			t.Fatalf("Type = %q, want %q", e.Type, client.ExporterImage)
		}
		if e.Attrs["name"] != "reg.example/app:dev" || e.Attrs["push"] != "true" {
			t.Fatalf("Attrs = %v", e.Attrs)
		}
	})

	t.Run("nothing to export", func(t *testing.T) {
		if _, err := (Output{}).exportEntry(); err == nil {
			t.Fatal("expected an error: a build with no output is a build nobody can use")
		}
	})

	t.Run("push without a name", func(t *testing.T) {
		if _, err := (Output{Push: true}).exportEntry(); err == nil {
			t.Fatal("expected an error: there is nowhere to push an unnamed image")
		}
	})
}
