package codegen

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/gpu"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
)

func TestSemanticGraphMatchesCurrentCompiler(t *testing.T) {
	profile := &gpu.Profile{Index: "https://jetson.example/simple", Runtime: []string{"nvidia-cuda-runtime-cu12"}, LibDir: "/opt/cuda/lib"}
	images := map[string]string{"python:3.12-slim": "sha256:python", "swift:6": "sha256:swift"}
	cases := []struct {
		name    string
		file    *spec.File
		profile *gpu.Profile
	}{
		{"linked pip overlay", &spec.File{Version: 1, Stages: []spec.Stage{{
			Name: "app", From: "python:3.12-slim", Workdir: "/app",
			Install: &spec.Install{
				Apt: &spec.AptInstall{Packages: []string{"libgomp1"}},
				Pip: []spec.PipInstall{{Requirements: "requirements.txt", BuildPackages: []string{"build-essential"}}},
			},
			Copy: []spec.CopyEntry{{From: "local", Paths: []string{"app.py"}}},
		}}}, nil},
		{"cuda runtime overlay", &spec.File{Version: 1, Stages: []spec.Stage{{
			Name: "app", From: "python:3.12-slim", CUDA: true,
			Install: &spec.Install{Pip: []spec.PipInstall{{Packages: []string{"torch"}, CUDA: true}}},
		}}}, profile},
		{"prior-stage inheritance and swift caches", &spec.File{Version: 1, Stages: []spec.Stage{
			{Name: "build", From: "swift:6", Workdir: "/src", Copy: []spec.CopyEntry{{From: "local", Paths: []string{"Package.swift", "Sources"}, Dest: "/src/"}}, Build: &spec.Build{Lang: "swift", Product: "server"}},
			{Name: "app", From: "build", Cmd: []string{".build/release/server"}},
		}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, err := Generate(tc.file, images, nil, "linux/arm64", tc.profile, WithCacheScope("/project"))
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			g, err := ir.Lower(tc.file, ir.Options{Images: images, Platform: "linux/arm64", CUDAProfile: tc.profile, CacheScope: "/project"})
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			got, err := GenerateGraph(g, images)
			if err != nil {
				t.Fatalf("GenerateGraph: %v", err)
			}
			if got != want {
				t.Fatalf("semantic graph drifted from current compiler:\n--- graph ---\n%s--- current ---\n%s", got, want)
			}
		})
	}
}
