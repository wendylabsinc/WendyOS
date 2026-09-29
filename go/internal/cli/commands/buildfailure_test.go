package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	if strings.Contains(got, "Cause: docker buildx build (OCI export) failed") {
		t.Errorf("summary fell back to the builder's exit status:\n%s", got)
	}
}

// The run's final error line (and a --wait-ready "failed" object's message,
// and the JSON error envelope) is the returned error's message: it names the
// failing step and its cause, not the builder's exit status.
func TestBuildFailureErrorNamesTheStepAndCause(t *testing.T) {
	const builderErr = "docker buildx build (OCI export) failed: exit status 1"
	for _, tc := range []struct {
		name string
		log  string
		want string
	}{
		{
			name: "go compile error: the step's last output line",
			log:  readBuildFailureFixture(t, "go-compile.log"),
			want: "build failed at [build 4/4] RUN go build -o /out/app .: ./main.go:6:14: undefined: foo",
		},
		{
			name: "python exit: the step's last output line",
			log:  readBuildFailureFixture(t, "python-exit.log"),
			want: `build failed at [py 2/2] RUN python -c "import sys; [print(f'line {i}', flush=True) for i in range(1,31)]; sys.exit(3)": line 30`,
		},
		{
			name: "interleaved stages: the failing step's own last line",
			log:  readBuildFailureFixture(t, "interleaved.log"),
			want: "build failed at [api 3/3] RUN go build -o /out/api ./cmd/api: internal/store/db.go:57:2: declared and not used: tx",
		},
		{
			name: "pip conflict: the summarized cause",
			log:  pipBuildFailureFixture,
			want: "build failed at [stagefile-pip-deps-0 5/7] RUN pip install …: pip dependency conflict: ultralytics/CLIP and ultralytics/mobileclip both report package metadata as unknown 0.0.0",
		},
		{
			name: "an ERROR: line: its message",
			log:  "#5 [3/5] COPY Package.swift .\n#5 ERROR: failed to compute cache key: \"/Package.swift\": not found\n",
			want: `build failed at [3/5] COPY Package.swift .: failed to compute cache key: "/Package.swift": not found`,
		},
		{
			name: "a cause without a step",
			log:  "#2 [internal] load metadata for docker.io/library/nope:latest\n#2 ERROR: docker.io/library/nope:latest: not found\n",
			want: "build failed: docker.io/library/nope:latest: not found",
		},
		{
			// The last step in the log succeeded; the push after it failed.
			name: "a failure outside any build step names no step",
			log:  "#8 [build 4/4] RUN go build -o /out/app .\n#8 DONE 3.1s\n\n#12 exporting to image\n#12 ERROR: failed to push localhost:5000/app:latest: connection reset by peer\n",
			want: "build failed: failed to push localhost:5000/app:latest: connection reset by peer",
		},
		{
			name: "nothing to go on: the original error",
			log:  "#1 [internal] load build definition from Dockerfile\n#1 DONE 0.0s\nERROR: failed to build: failed to solve: exit status 1\n",
			want: builderErr,
		},
		{
			name: "no log at all: the original error",
			want: builderErr,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buildErr := &imageBuildFailedError{errors.New(builderErr)}
			err := buildFailureError(summarizeBuildFailure(tc.log, buildErr), buildErr)
			if err.Error() != tc.want {
				t.Fatalf("error = %q\nwant    %q", err, tc.want)
			}
			var imageErr *imageBuildFailedError
			if !errors.As(err, &imageErr) || imageErr != buildErr || !errors.Is(err, buildErr) || !isImageBuildFailure(err) {
				t.Fatalf("error %q lost the build failure it wraps", err)
			}
			if ErrorClass(err) != "build_failed" {
				t.Fatalf("class = %q, want build_failed", ErrorClass(err))
			}
			if isChunkDeployCancellation(context.Background(), err) {
				t.Fatal("a build failure reads as a cancellation")
			}
		})
	}
}

// The rewrite keeps every classification the original error had, and never
// touches a cancellation.
func TestBuildFailureErrorKeepsTheChain(t *testing.T) {
	log := readBuildFailureFixture(t, "go-compile.log")
	const want = "build failed at [build 4/4] RUN go build -o /out/app .: ./main.go:6:14: undefined: foo"

	classified := commandErrorf(errBuildFailed, "build failed: exit status 1")
	if err := buildFailureError(summarizeBuildFailure(log, classified), classified); err.Error() != want || !errors.Is(err, errBuildFailed) || ErrorClass(err) != "build_failed" {
		t.Fatalf("classified: err = %q (class %q)", err, ErrorClass(err))
	}
	missingTool := &imageBuildFailedError{fmt.Errorf("docker buildx build (OCI export) failed: %w", exec.ErrNotFound)}
	if err := buildFailureError(summarizeBuildFailure(log, missingTool), missingTool); err.Error() != want || ErrorClass(err) != "tool_not_found" {
		t.Fatalf("missing tool: err = %q (class %q), want tool_not_found kept", err, ErrorClass(err))
	}
	for _, cancelled := range []error{ErrUserCancelled, fmt.Errorf("docker buildx build: %w", context.Canceled)} {
		if err := buildFailureError(summarizeBuildFailure(log, cancelled), cancelled); err != cancelled {
			t.Fatalf("cancellation %v was rewritten to %q", cancelled, err)
		}
	}
	if err := buildFailureError(summarizeBuildFailure(log, nil), nil); err != nil {
		t.Fatalf("no error became %v", err)
	}
}

// renderBuildFailure prints the details block and returns the rewritten error.
func TestRenderBuildFailureReturnsTheStepAndCause(t *testing.T) {
	original := persistBuildFailureLog
	defer func() { persistBuildFailureLog = original }()
	persistBuildFailureLog = func(string, string) (string, error) { return "/tmp/wendy-build-image-1.log", nil }

	var out strings.Builder
	buildErr := &imageBuildFailedError{errors.New("docker buildx build (OCI export) failed: exit status 1")}
	err := renderBuildFailure(&out, "", readBuildFailureFixture(t, "go-compile.log"), buildErr)
	if err.Error() != "build failed at [build 4/4] RUN go build -o /out/app .: ./main.go:6:14: undefined: foo" || !errors.Is(err, buildErr) {
		t.Fatalf("err = %q", err)
	}
	for _, want := range []string{
		"  Step: build 4/4 — RUN go build -o /out/app .\n",
		// The builder's own error stays visible above the rewritten one.
		"  Error: docker buildx build (OCI export) failed: exit status 1\n",
		"  Build log: /tmp/wendy-build-image-1.log\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("details block missing %q:\n%s", want, out.String())
		}
	}
}

// Only an image build's failure is rewritten. Any other failure the details
// block summarizes — the builder could not be set up, a remote build host
// failed — keeps its own message, and the details block shows it once.
func TestBuildFailureErrorRewritesOnlyImageBuildFailures(t *testing.T) {
	original := persistBuildFailureLog
	defer func() { persistBuildFailureLog = original }()
	persistBuildFailureLog = func(string, string) (string, error) { return "/tmp/wendy-build-image-1.log", nil }
	log := readBuildFailureFixture(t, "go-compile.log")
	for _, buildErr := range []error{
		classifyCommandError(errBuilderUnavailable, errors.New(`bootstrapping buildx builder "wendy-oci": exit status 1`)),
		errors.New("pushing the build context to spark-office: connection reset by peer"),
	} {
		var out strings.Builder
		err := renderBuildFailure(&out, "", log, buildErr)
		if err != buildErr {
			t.Errorf("%q was rewritten to %q", buildErr, err)
		}
		if got := strings.Count(out.String(), buildErr.Error()); got != 1 {
			t.Errorf("details block shows %q %d times, want once:\n%s", buildErr, got, out.String())
		}
	}
}
