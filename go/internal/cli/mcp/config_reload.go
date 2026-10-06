package mcp

import "github.com/wendylabsinc/wendy/go/internal/shared/config"

// reloadConfigFn re-reads config.json for handlers that must not act on the
// startup snapshot. Nil — the default, and what every test that builds a
// server from an in-memory Config gets — means "use s.cfg". A package variable
// rather than a server field: one server runs per process, and it keeps this
// change off mcpServer's struct.
var reloadConfigFn func() (*config.Config, error)

// EnableConfigReload makes cloud tools read credentials from config.json on
// each call instead of from the snapshot `wendy mcp serve` loaded at startup.
// The server is long-lived: a `wendy auth login` in another terminal must be
// usable without restarting the AI tool. Call it once, before Start.
func EnableConfigReload(load func() (*config.Config, error)) {
	reloadConfigFn = load
}

// currentConfig returns the config as it is on disk now when reloading is
// enabled, falling back to the startup snapshot if it cannot be read.
func (s *mcpServer) currentConfig() *config.Config {
	if reloadConfigFn == nil {
		return s.cfg
	}
	cfg, err := reloadConfigFn()
	if err != nil || cfg == nil {
		return s.cfg
	}
	return cfg
}
