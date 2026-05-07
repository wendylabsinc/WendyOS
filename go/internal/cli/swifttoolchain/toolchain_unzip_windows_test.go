//go:build windows

package swifttoolchain

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestUnzipOverwriteEnv_WindowsNoOp(t *testing.T) {
	want := os.Environ()

	env, cleanup, err := unzipOverwriteEnv()
	if err != nil {
		t.Fatalf("unzipOverwriteEnv() error = %v, want nil", err)
	}
	if cleanup == nil {
		t.Fatal("unzipOverwriteEnv() cleanup = nil, want non-nil no-op")
	}
	defer cleanup()

	// Must return a non-nil env so callers can assign cmd.Env without
	// nil-checking, and that env must be the unmodified process environment
	// (no PATH wrapper directory injected).
	if env == nil {
		t.Fatal("unzipOverwriteEnv() env = nil, want process environment")
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("unzipOverwriteEnv() env differs from process environment\n got = %v\nwant = %v", env, want)
	}
	for _, kv := range env {
		if !strings.HasPrefix(strings.ToUpper(kv), "PATH=") {
			continue
		}
		if strings.Contains(kv, "wendy-unzip-") {
			t.Fatalf("unzipOverwriteEnv() injected wendy-unzip shim into PATH: %q", kv)
		}
	}
}
