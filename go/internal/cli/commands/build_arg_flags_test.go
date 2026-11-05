package commands

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestRunBuildArgFlagForwarding(t *testing.T) {
	cmd := newRunCmd()
	if err := cmd.ParseFlags([]string{
		"--skip-cloud-registration",
		"--build-arg", "WENDY_VERSION=old",
		"--build-arg", "WENDY_VERSION=2026.11.03-074612",
		"--build-arg", "OPTIONS=a,b=c with spaces",
		"--build-arg", "EMPTY=",
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := cmd.Flags().GetStringArray("build-arg")
	if err != nil {
		t.Fatal(err)
	}
	defaults := map[string]string{"WENDY_PLATFORM": "generic", "WENDY_VERSION": "default"}
	args, err := mergeBuildArgFlags(defaults, entries)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"WENDY_PLATFORM": "generic",
		"WENDY_VERSION":  "2026.11.03-074612",
		"OPTIONS":        "a,b=c with spaces",
		"EMPTY":          "",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("build args = %#v, want %#v", args, want)
	}
	if defaults["WENDY_VERSION"] != "default" {
		t.Fatal("merge mutated defaults")
	}
	// Cover the OCI export used by the reported failure and BuildKit's frontend.
	oci := buildxOCIExportArgs("builder", "linux/arm64", "Dockerfile", "", "image.tar", false, args)
	kit := buildkitOCIArgs("/work", "/work", "Dockerfile", "linux/arm64", args, "image.tar")
	for key, value := range want {
		pair := key + "=" + value
		i := slices.Index(oci, pair)
		if i < 1 || oci[i-1] != "--build-arg" {
			t.Errorf("OCI export missing argument %q: %v", pair, oci)
		}
		if !slices.Contains(kit, "build-arg:"+pair) {
			t.Errorf("BuildKit missing argument %q: %v", pair, kit)
		}
	}
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM scratch\nARG WENDY_VERSION\n")
	before := hashOrFatal(t, dir, args)
	args, err = mergeBuildArgFlags(args, []string{"WENDY_VERSION=next"})
	if err != nil {
		t.Fatal(err)
	}
	if hashOrFatal(t, dir, args) == before {
		t.Fatal("changing the CLI build argument did not invalidate the build hash")
	}
}

func TestRunBuildArgRejectsInvalidBeforeProjectSetup(t *testing.T) {
	for _, entry := range []string{"MISSING_VALUE", "=secret", "BAD-NAME=secret", "NAME=-secret", "NAME=secret\nline"} {
		t.Run(entry, func(t *testing.T) {
			cmd := newRunCmd()
			cmd.SetContext(context.Background())
			if err := cmd.ParseFlags([]string{"--build-arg", entry, "--prefix", "/nonexistent-project"}); err != nil {
				t.Fatal(err)
			}
			err := cmd.RunE(cmd, nil)
			if !IsUsageError(err) || !strings.Contains(err.Error(), "--build-arg") {
				t.Fatalf("expected build argument usage error before project setup, got %v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error exposed argument value: %v", err)
			}
		})
	}
}

func TestRunBuildArgRejectsDirectLLB(t *testing.T) {
	err := runCommand(context.Background(), runOptions{
		prefix: t.TempDir(), stagefileBackend: "llb", buildArgs: []string{"VERSION=1"},
	})
	if !IsUsageError(err) || !strings.Contains(err.Error(), "--stagefile-backend=dockerfile") {
		t.Fatalf("expected unsupported backend usage error, got %v", err)
	}
}
