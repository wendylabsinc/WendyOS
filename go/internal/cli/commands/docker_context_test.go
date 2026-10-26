package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerContextSurvivesCleanConfig(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("WENDY_BUILDX_BUILDER", "")
	file := filepath.Join(os.Getenv("DOCKER_CONFIG"), "config.json")
	if err := os.WriteFile(file, []byte(`{"currentContext":"desktop-linux","credsStore":"secret-helper"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	desktop := ociBuilderName()
	if err := os.WriteFile(file, []byte(`{"currentContext":"colima"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	colima := ociBuilderName()
	if desktop == colima {
		t.Fatal("different daemons reused the same automatic builder")
	}
	if colima != ociBuilderName() {
		t.Fatal("builder selection is not stable")
	}
	env := strings.Join(dockerConfigEnv("/clean/config"), "\n")
	if !strings.Contains(env, "DOCKER_CONTEXT=colima\n") {
		t.Fatalf("active context lost: %s", env)
	}
	if !strings.Contains(env, "DOCKER_CONFIG=/clean/config") {
		t.Fatal("clean config lost")
	}
	t.Setenv("DOCKER_HOST", "unix:///tmp/explicit.sock")
	ctx, id := dockerSelection()
	if ctx != "" || id != "host:unix:///tmp/explicit.sock" {
		t.Fatalf("host override: %q %q", ctx, id)
	}
	t.Setenv("DOCKER_CONTEXT", "explicit-context")
	ctx, _ = dockerSelection()
	if ctx != "explicit-context" {
		t.Fatal("explicit context must override host")
	}
	t.Setenv("WENDY_BUILDX_BUILDER", "my-builder")
	if got := ociBuilderName(); got != "my-builder-oci" {
		t.Fatalf("explicit builder renamed: %s", got)
	}
}
