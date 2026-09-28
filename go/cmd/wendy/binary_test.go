package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runMainEnv makes the test binary run main() instead of the tests, so the
// tests below exercise the real process: exit status, stdout, and stderr.
const runMainEnv = "WENDY_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main() // exits
	}
	os.Exit(m.Run())
}

// runWendy runs this binary as wendy with args, without a terminal, in an
// empty config, and returns its exit status, stdout and stderr.
func runWendy(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		runMainEnv+"=1",
		"HOME="+dir,
		"WENDY_CONFIG_DIR="+dir,
		"WENDY_ANALYTICS=false",
		// Coverage builds write here instead of warning on stderr.
		"GOCOVERDIR="+t.TempDir(),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running wendy %q: %v", args, err)
	}
	return code, stdout.String(), stderr.String()
}

// The contract an agent relies on, checked on the real process: a failure
// leaves stdout empty, writes exactly one stderr line, and exits with the
// class's status. Without a terminal JSON mode is automatic.
func TestFailuresFromTheRealProcess(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		exit int
		code string
	}{
		{"unknown flag", []string{"--json", "device", "info", "--bogus"}, 2, "cli_usage"},
		{"unknown command, JSON by itself", []string{"banana"}, 2, "cli_usage"},
		{"unknown subcommand", []string{"device", "banana"}, 2, "cli_usage"},
		{"invalid flag value", []string{"build", "--builder", "nonsense"}, 2, "cli_usage"},
		{"not logged in", []string{"cloud", "discover"}, 3, "auth_required"},
		{"nothing listening", []string{"--device", "127.0.0.1:1", "device", "info"}, 5, "device_unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runWendy(t, tc.args...)
			if code != tc.exit {
				t.Errorf("exit = %d, want %d; stderr = %q", code, tc.exit, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing", stdout)
			}
			if strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
				t.Fatalf("stderr is not exactly one line: %q", stderr)
			}
			var env errorEnvelope
			if err := json.Unmarshal([]byte(stderr), &env); err != nil {
				t.Fatalf("stderr is not an envelope: %v\n%s", err, stderr)
			}
			if env.Error.Code != tc.code || env.Error.Exit != tc.exit || env.Error.Message == "" {
				t.Errorf("envelope = %+v, want code %q and exit %d", env.Error, tc.code, tc.exit)
			}
		})
	}

	t.Run("--json=false keeps text", func(t *testing.T) {
		code, stdout, stderr := runWendy(t, "--json=false", "--device", "127.0.0.1:1", "device", "info")
		if code != 5 || stdout != "" || !strings.HasPrefix(stderr, "✗ Could not connect to device at 127.0.0.1:1.") {
			t.Errorf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
	})
}
