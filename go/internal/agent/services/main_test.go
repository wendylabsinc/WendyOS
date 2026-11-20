package services

import (
	"os"
	"testing"
)

// This package's tests run stand-in pipelines out of t.TempDir, which only the
// user running the tests can search. When that user is root, a sandboxed child
// could not reach them, so NewVideoService builds no sandbox here. The sandbox
// tests build their own.
func TestMain(m *testing.M) {
	os.Setenv(cameraSandboxEnv, "off") //nolint:errcheck
	os.Exit(m.Run())
}
