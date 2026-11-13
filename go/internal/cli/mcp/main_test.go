package mcp

import (
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
)

// TestMain opts the package in to plaintext cloud dials: the fake clouds these
// tests start are plaintext gRPC servers, exactly the local-dev case
// WENDY_CLOUD_INSECURE exists for. The TLS default is pinned in clouddefaults.
func TestMain(m *testing.M) {
	os.Setenv(clouddefaults.CloudInsecureEnv, "true")
	os.Exit(m.Run())
}
