package mcp

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// newTestServer keeps unit-test configuration isolated in memory. Production
// servers are always constructed with New, whose store is config.json.
func newTestServer(cfg *config.Config, connectFn ConnectFunc) *mcpServer {
	if cfg == nil {
		cfg = &config.Config{}
	}
	return newMCPServer(
		func() (*config.Config, error) { return cfg, nil },
		func(*config.Config) error { return nil },
		connectFn,
	)
}

func useDiskConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
}
