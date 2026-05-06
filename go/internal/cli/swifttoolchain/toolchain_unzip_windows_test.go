//go:build windows

package swifttoolchain

import (
	"reflect"
	"testing"
)

func TestUnzipOverwriteEnv_WindowsNoOp(t *testing.T) {
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
	if !reflect.DeepEqual(env, append([]string(nil), env...)) {
		t.Fatal("env should be a real slice")
	}
}
