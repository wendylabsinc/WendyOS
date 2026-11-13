package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeCommandOnPath makes PATH contain only a dir holding an executable
// named name, so isCommandAvailable(name) is true without the real tool.
func fakeCommandOnPath(t *testing.T, name string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake PATH executables are shell scripts")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing fake %s: %v", name, err)
	}
	t.Setenv("PATH", dir)
}

// chdirTemp switches the working directory to a fresh temp dir for the test.
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	return dir
}

// Without a TTY and without --assistant, `wendy init` used to open the
// assistant picker after scaffolding and fail with "picker: could not open a
// new TTY" (exit 1); a retry then failed with "wendy.json already exists".
func TestInitCommand_NonInteractiveWithoutAssistantFlagSkipsPicker(t *testing.T) {
	dir := chdirTemp(t)
	stubNonInteractive(t)
	fakeCommandOnPath(t, "claude")

	cmd := newInitCmd()
	cmd.SetArgs([]string{
		"--app-id", "demo-app",
		"--target", "wendyos",
		"--language", "python",
		"--no-extra-entitlements",
	})

	var execErr error
	stderr := captureStderr(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("Execute: %v\nstderr:\n%s", execErr, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "wendy.json")); err != nil {
		t.Fatalf("wendy.json not created: %v", err)
	}
	if n := strings.Count(stderr, "--assistant"); n != 1 {
		t.Fatalf("want exactly one line mentioning --assistant, got %d in:\n%s", n, stderr)
	}
}

// dockerfileRunLines returns the shell of every RUN instruction in a
// single-line-RUN Dockerfile.
func dockerfileRunLines(dockerfile string) []string {
	var runs []string
	for _, line := range strings.Split(dockerfile, "\n") {
		if rest, ok := strings.CutPrefix(line, "RUN "); ok {
			runs = append(runs, rest)
		}
	}
	return runs
}

// runDockerfileRUNs runs each RUN line with /bin/sh in dir, with a fake `uv`
// that fails exactly like the real one when --frozen is passed without a
// uv.lock, and logs every call to dir/uv.calls.
func runDockerfileRUNs(t *testing.T, dir string, runs []string) string {
	t.Helper()
	bin := t.TempDir()
	fakeUV := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$a\" = \"--frozen\" ] && [ ! -f uv.lock ]; then\n" +
		"    echo 'error: Unable to find lockfile at `uv.lock`, but `--frozen` was provided' >&2\n" +
		"    exit 2\n" +
		"  fi\n" +
		"done\n" +
		"echo \"uv $*\" >> uv.calls\n"
	if err := os.WriteFile(filepath.Join(bin, "uv"), []byte(fakeUV), 0o755); err != nil {
		t.Fatalf("writing fake uv: %v", err)
	}
	_ = os.Remove(filepath.Join(dir, "uv.calls"))
	for _, run := range runs {
		c := exec.Command("/bin/sh", "-c", run)
		c.Dir = dir
		c.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("RUN %s: %v\n%s", run, err, out)
		}
	}
	calls, err := os.ReadFile(filepath.Join(dir, "uv.calls"))
	if err != nil {
		t.Fatalf("reading uv.calls: %v", err)
	}
	return string(calls)
}

// A fresh Python scaffold has no uv.lock. The generated Dockerfile used to
// run `uv sync --frozen` unconditionally, so `wendy run` failed with "Unable
// to find lockfile at `uv.lock`, but `--frozen` was provided".
func TestInitPythonUVProject_DockerfileBuildsWithAndWithoutLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executes the Dockerfile's RUN lines with /bin/sh")
	}
	dir := t.TempDir()
	if err := initPythonUVProject(dir, "demo-app"); err != nil {
		t.Fatalf("initPythonUVProject: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatalf("reading Dockerfile: %v", err)
	}
	runs := dockerfileRunLines(string(data))
	if len(runs) != 2 {
		t.Fatalf("want 2 RUN lines, got %d:\n%s", len(runs), data)
	}

	// Fresh scaffold: no lock, so no --frozen anywhere.
	if calls := runDockerfileRUNs(t, dir, runs); strings.Contains(calls, "--frozen") {
		t.Fatalf("uv ran with --frozen but there is no uv.lock:\n%s", calls)
	}

	// Committed lock: both syncs stay reproducible.
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte("version = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if calls := runDockerfileRUNs(t, dir, runs); strings.Count(calls, "--frozen") != 2 {
		t.Fatalf("want both uv syncs --frozen with a uv.lock present, got:\n%s", calls)
	}
}

// The Dockerfile's CMD is `uv run <pkg>`, a [project.scripts] entry point.
// uv only installs entry points for a packaged project, i.e. one with a
// [build-system]; without it the built image exits at once with
// "Failed to spawn: `<pkg>`". The CMD must also pass --no-sync: a packaged
// project is installed editable, uv judges its freshness by file ctimes that
// image layers never preserve, so a plain `uv run` rebuilds the package on
// every container start — which needs PyPI for hatchling and fails offline.
func TestInitPythonUVProject_PyprojectIsAPackagedProject(t *testing.T) {
	dir := t.TempDir()
	if err := initPythonUVProject(dir, "demo-app"); err != nil {
		t.Fatalf("initPythonUVProject: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "pyproject.toml"))
	if err != nil {
		t.Fatalf("reading pyproject.toml: %v", err)
	}
	for _, want := range []string{
		"[project.scripts]\ndemo_app = \"demo_app:main\"",
		"[build-system]",
		`build-backend = "hatchling.build"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("pyproject.toml missing %q:\n%s", want, data)
		}
	}
	dockerfile, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatalf("reading Dockerfile: %v", err)
	}
	if !strings.Contains(string(dockerfile), `CMD ["uv", "run", "--no-sync", "demo_app"]`) {
		t.Errorf("Dockerfile CMD should run the demo_app entry point without re-syncing at start:\n%s", dockerfile)
	}
}

// Without a TTY, each question `wendy init` would ask with a picker or
// checklist must instead fail — before anything is scaffolded — naming the
// flag that answers it. They used to fail with "could not open a new TTY".
func TestInitCommand_NonInteractiveMissingAnswersNameTheFlag(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantFlag string
	}{
		{"entitlements", []string{"--app-id", "demo-app", "--target", "wendyos", "--language", "python"}, "--entitlement"},
		{"language", []string{"--app-id", "demo-app", "--target", "wendyos", "--no-extra-entitlements"}, "--language"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := chdirTemp(t)
			stubNonInteractive(t)

			cmd := newInitCmd()
			cmd.SetArgs(tc.args)
			var execErr error
			stderr := captureStderr(t, func() { execErr = cmd.Execute() })
			if execErr == nil || !strings.Contains(execErr.Error(), tc.wantFlag) {
				t.Fatalf("err = %v, want one naming %s\nstderr:\n%s", execErr, tc.wantFlag, stderr)
			}
			if strings.Contains(execErr.Error(), "TTY") {
				t.Fatalf("err = %v, want a usage error, not a TTY failure", execErr)
			}
			if _, err := os.Stat(filepath.Join(dir, "wendy.json")); err == nil {
				t.Fatal("wendy.json was written; the error must come before scaffolding")
			}
		})
	}
}
