package stagefile

import (
	"reflect"
	"runtime"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
)

func lowerForTest(t *testing.T, f *spec.File) *ir.Graph {
	t.Helper()
	g, err := ir.Lower(f, ir.Options{Platform: "linux/arm64"})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	return g
}

// A `platform: build` stage's base image config must be resolved at the build
// platform, not the target, or Emit's platform check rejects it. configRefsByPlatform
// routes build-exclusive refs to the build set and everything else to the target.
func TestConfigRefsByPlatformSplitsBuildStages(t *testing.T) {
	f := &spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "assets", From: "node:20", Platform: "build",
			Install: &spec.Install{Apt: &spec.AptInstall{Packages: []string{"make"}}}},
		{Name: "app", From: "python:3.12-slim",
			Copy: []spec.CopyEntry{{From: "assets", Paths: []string{"/web"}, Dest: "/web"}},
			Cmd:  []string{"/app"}},
	}}
	g := lowerForTest(t, f)

	target, build := configRefsByPlatform(g)
	if !reflect.DeepEqual(target, []string{"python:3.12-slim"}) {
		t.Fatalf("target refs = %v, want [python:3.12-slim]", target)
	}
	if !reflect.DeepEqual(build, []string{"node:20"}) {
		t.Fatalf("build refs = %v, want [node:20] (the platform: build stage)", build)
	}
}

// pin: false bases carry no registry digest, so they are excluded from the
// registry-resolved sets and collected separately for local-daemon resolution.
func TestPinFalseRefsAreCollectedSeparately(t *testing.T) {
	f := &spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "app", From: "mlx-server:0.1", Pin: new(bool), Cmd: []string{"/serve"}},
	}}
	g := lowerForTest(t, f)

	if refs := pinFalseRefs(g); !reflect.DeepEqual(refs, []string{"mlx-server:0.1"}) {
		t.Fatalf("pinFalseRefs = %v, want [mlx-server:0.1]", refs)
	}
	// A pin: false ref must not appear among the registry-resolved refs.
	target, build := configRefsByPlatform(g)
	if len(target) != 0 || len(build) != 0 {
		t.Fatalf("configRefsByPlatform = (%v, %v), want empty — pin: false is resolved locally", target, build)
	}
}

// The build platform is Linux on the host architecture, never the actual host
// OS: buildkitd runs Linux, so a darwin build platform names an OS no daemon can
// satisfy.
func TestBuildPlatformIsLinuxHostArch(t *testing.T) {
	if got, want := buildPlatform(), "linux/"+runtime.GOARCH; got != want {
		t.Fatalf("buildPlatform() = %q, want %q", got, want)
	}
}
