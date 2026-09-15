package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// TestStagefileDirectLLBIntegration is an opt-in live BuildKit check and
// reproducible microbenchmark for the production OCI-directory hook. Ordinary
// unit tests skip it because it requires Docker, a registry pull, and a running
// buildx daemon. Run with WENDY_LLB_INTEGRATION=1.
func TestStagefileDirectLLBIntegration(t *testing.T) {
	if os.Getenv("WENDY_LLB_INTEGRATION") != "1" {
		t.Skip("set WENDY_LLB_INTEGRATION=1 to run the live direct-LLB comparison")
	}

	const source = `version: 1
stages:
  - name: app
    from: alpine:3.20
    workdir: /app
    env:
      MODE: integration
    copy:
      - from: local
        paths: [app.txt]
    cmd: [cat, app.txt]
`
	makeProject := func(name string) (string, string) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "build.stagefile.yaml"), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("initial\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		generated, err := compileStagefile(dir, "build.stagefile.yaml", "")
		if err != nil {
			t.Fatal(err)
		}
		return dir, generated
	}

	llbDir, llbDockerfile := makeProject("llb")
	dockerDir, dockerfile := makeProject("dockerfile")
	llbLayout := filepath.Join(t.TempDir(), "layout")
	dockerLayout := filepath.Join(t.TempDir(), "layout")
	llbCtx := withStagefileBackend(context.Background(), stagefileBackendLLBValue)
	dockerCtx := withStagefileBackend(context.Background(), stagefileBackendDockerfileValue)

	build := func(ctx context.Context, dir, file, layout string) time.Duration {
		t.Helper()
		start := time.Now()
		progress := io.Writer(io.Discard)
		if testing.Verbose() {
			progress = os.Stderr
		}
		if err := buildImageToOCILayoutDirWithDocker(ctx, dir, file, "linux/arm64", nil, layout, progress, progress); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}

	llbCold := build(llbCtx, llbDir, llbDockerfile, llbLayout)
	dockerCold := build(dockerCtx, dockerDir, dockerfile, dockerLayout)

	var llbWarm, dockerWarm []time.Duration
	for i := 0; i < 6; i++ {
		content := []byte(fmt.Sprintf("edit-%d\n", i))
		if err := os.WriteFile(filepath.Join(llbDir, "app.txt"), content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dockerDir, "app.txt"), content, 0o644); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			llbWarm = append(llbWarm, build(llbCtx, llbDir, llbDockerfile, llbLayout))
			dockerWarm = append(dockerWarm, build(dockerCtx, dockerDir, dockerfile, dockerLayout))
		} else {
			dockerWarm = append(dockerWarm, build(dockerCtx, dockerDir, dockerfile, dockerLayout))
			llbWarm = append(llbWarm, build(llbCtx, llbDir, llbDockerfile, llbLayout))
		}
	}

	_, llbConfig, err := readOCILayoutDirLayers(llbLayout, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	_, dockerConfig, err := readOCILayoutDirLayers(dockerLayout, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	type runtimeConfig struct {
		Config struct {
			Env         []string       `json:"Env"`
			WorkingDir  string         `json:"WorkingDir"`
			Entrypoint  []string       `json:"Entrypoint"`
			Cmd         []string       `json:"Cmd"`
			User        string         `json:"User"`
			Healthcheck map[string]any `json:"Healthcheck"`
		} `json:"config"`
	}
	decode := func(raw []byte) runtimeConfig {
		t.Helper()
		var cfg runtimeConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	if got, want := decode(llbConfig), decode(dockerConfig); !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime config differs:\nLLB: %#v\nDockerfile: %#v", got, want)
	}

	median := func(values []time.Duration) time.Duration {
		values = append([]time.Duration(nil), values...)
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		return (values[len(values)/2-1] + values[len(values)/2]) / 2
	}
	t.Logf("cold: direct LLB %s, Dockerfile %s", llbCold.Round(time.Millisecond), dockerCold.Round(time.Millisecond))
	t.Logf("warm source-edit median (n=%d): direct LLB %s, Dockerfile %s", len(llbWarm), median(llbWarm).Round(time.Millisecond), median(dockerWarm).Round(time.Millisecond))
}
