package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pipBuildFailureFixture = `#15 8.239 ERROR: Cannot install unknown 0.0.0 (from git+https://github.com/ultralytics/CLIP.git@488e81a6711eea7346872b46ea928b367da8889d) and unknown 0.0.0 (from git+https://github.com/ultralytics/mobileclip.git@a17446aaa1860d25cab8531ec27c3ecac3c05bf5) because these package versions have conflicting dependencies.
#15 8.239 ERROR: ResolutionImpossible: for help visit https://pip.pypa.io/en/latest/topics/dependency-resolution/
#15 ERROR: process "/bin/sh -c pip install lots-of-packages" did not complete successfully: exit code: 1
------
 > [stagefile-pip-deps-0 5/7] RUN --mount=type=cache,sharing=locked,id=stagefile-pip-96a296d224f285c6,target=/root/.cache/pip pip install fastapi httpx numpy opencv-python pydantic uvicorn ultralytics:
------
Dockerfile.generated.yolo:12
--------------------
ERROR: failed to build: failed to solve: process "/bin/sh -c pip install lots-of-packages" did not complete successfully: exit code: 1
View build details: docker-desktop://dashboard/build/wendy-oci/example
`

func TestSummarizeBuildFailurePipConflict(t *testing.T) {
	got := summarizeBuildFailure(pipBuildFailureFixture, errors.New("docker buildx build failed: exit status 1"))
	if got.cause != "pip dependency conflict: ultralytics/CLIP and ultralytics/mobileclip both report package metadata as unknown 0.0.0" {
		t.Errorf("cause = %q", got.cause)
	}
	if got.step != "stagefile-pip-deps-0 5/7 — RUN pip install …" {
		t.Errorf("step = %q", got.step)
	}
	if got.source != "Dockerfile.generated.yolo:12" {
		t.Errorf("source = %q", got.source)
	}
	if got.detailsURL != "docker-desktop://dashboard/build/wendy-oci/example" {
		t.Errorf("detailsURL = %q", got.detailsURL)
	}
}

func TestSummarizeBuildFailureGenericError(t *testing.T) {
	log := "#5 [3/5] COPY Package.swift .\n#5 ERROR: failed to compute cache key: \"/Package.swift\": not found\n"
	got := summarizeBuildFailure(log, errors.New("container build failed"))
	if got.cause != `failed to compute cache key: "/Package.swift": not found` {
		t.Errorf("cause = %q", got.cause)
	}
	if got.step != "3/5 — COPY Package.swift ." {
		t.Errorf("step = %q", got.step)
	}
}

func TestRenderBuildFailureShowsSummaryAndRetainsFullLog(t *testing.T) {
	original := persistBuildFailureLog
	defer func() { persistBuildFailureLog = original }()
	var savedLabel, savedLog string
	persistBuildFailureLog = func(label, raw string) (string, error) {
		savedLabel, savedLog = label, raw
		return "/tmp/wendy-build-yolo-fruits-123.log", nil
	}

	var out strings.Builder
	renderBuildFailure(&out, "yolo-fruits", pipBuildFailureFixture, errors.New("exit status 1"))
	got := out.String()
	for _, want := range []string{
		"Build failure details: yolo-fruits",
		"Cause: pip dependency conflict",
		"At: Dockerfile.generated.yolo:12",
		"Details: docker-desktop://dashboard/build/wendy-oci/example",
		"Build log: /tmp/wendy-build-yolo-fruits-123.log",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ResolutionImpossible") {
		t.Errorf("verbose raw log leaked into summary:\n%s", got)
	}
	if savedLabel != "yolo-fruits" || savedLog != pipBuildFailureFixture {
		t.Errorf("saved label/log = %q/%q", savedLabel, savedLog)
	}
}

func TestRenderBuildFailureFallsBackToRawWhenLogCannotBeSaved(t *testing.T) {
	original := persistBuildFailureLog
	defer func() { persistBuildFailureLog = original }()
	persistBuildFailureLog = func(string, string) (string, error) {
		return "", errors.New("temporary directory unavailable")
	}

	var out strings.Builder
	renderBuildFailure(&out, "api", "raw diagnostic\n", errors.New("exit status 1"))
	if !strings.Contains(out.String(), "raw diagnostic") {
		t.Fatalf("raw fallback missing:\n%s", out.String())
	}
}

func TestSanitizeBuildLogLabel(t *testing.T) {
	if got := sanitizeBuildLogLabel("API / Prod"); got != "api---prod" {
		t.Errorf("sanitizeBuildLogLabel = %q", got)
	}
}

func readBuildFailureFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "buildfailure", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The logs below are real `docker buildx build --progress plain` output
// (testdata/buildfailure/*.log), except interleaved.log, which reproduces two
// parallel stages writing between each other the way a docker-container
// builder does. BuildKit logs only its wrapper error for a failing RUN, so the
// cause used to fall back to "docker buildx build (OCI export) failed: exit
// status 1" (WDY-1832).
func TestSummarizeBuildFailureUsesFailingStepOutput(t *testing.T) {
	for _, tc := range []struct {
		fixture   string
		step      string
		cause     string
		output    []string
		absentOut string
	}{
		{
			fixture: "go-compile.log",
			step:    "build 4/4 — RUN go build -o /out/app .",
			cause:   "step failed with exit code 1",
			output:  []string{"# example.com/gofail", "./main.go:6:14: undefined: foo"},
		},
		{
			fixture: "interleaved.log",
			step:    "api 3/3 — RUN go build -o /out/api ./cmd/api",
			cause:   "step failed with exit code 1",
			output: []string{
				"# example.com/api/internal/store",
				"internal/store/db.go:41:9: cannot use rows (variable of type *sql.Rows) as []Row value in return statement",
				"internal/store/db.go:57:2: declared and not used: tx",
			},
			absentOut: "npm",
		},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			got := summarizeBuildFailure(readBuildFailureFixture(t, tc.fixture), errors.New("docker buildx build (OCI export) failed: exit status 1"))
			if got.step != tc.step {
				t.Errorf("step = %q, want %q", got.step, tc.step)
			}
			if got.cause != tc.cause {
				t.Errorf("cause = %q, want %q", got.cause, tc.cause)
			}
			if strings.Join(got.output, "\n") != strings.Join(tc.output, "\n") {
				t.Errorf("output = %q, want %q", got.output, tc.output)
			}
			if tc.absentOut != "" && strings.Contains(strings.Join(got.output, "\n"), tc.absentOut) {
				t.Errorf("output %q mixes in another step's %q lines", got.output, tc.absentOut)
			}
		})
	}
}

func TestSummarizeBuildFailureKeepsTheLastTwentyOutputLines(t *testing.T) {
	got := summarizeBuildFailure(readBuildFailureFixture(t, "python-exit.log"), errors.New("exit status 1"))
	if got.cause != "step failed with exit code 3" {
		t.Errorf("cause = %q", got.cause)
	}
	if len(got.output) != maxBuildFailureOutputLines || got.output[0] != "line 11" || got.output[len(got.output)-1] != "line 30" {
		t.Errorf("output = %q, want line 11 through line 30", got.output)
	}
	if !strings.HasPrefix(got.step, "py 2/2 — RUN python -c") {
		t.Errorf("step = %q", got.step)
	}
}

func TestRenderBuildFailureShowsTheFailingStepOutput(t *testing.T) {
	original := persistBuildFailureLog
	defer func() { persistBuildFailureLog = original }()
	persistBuildFailureLog = func(string, string) (string, error) { return "/tmp/wendy-build-image-1.log", nil }

	var out strings.Builder
	renderBuildFailure(&out, "", readBuildFailureFixture(t, "go-compile.log"), errors.New("docker buildx build (OCI export) failed: exit status 1"))
	got := out.String()
	for _, want := range []string{
		"  Step: build 4/4 — RUN go build -o /out/app .\n",
		"  Cause: step failed with exit code 1; its output ended with:\n",
		"    ./main.go:6:14: undefined: foo\n",
		"  At: Dockerfile:4\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "OCI export) failed") {
		t.Errorf("summary fell back to the builder's exit status:\n%s", got)
	}
}
