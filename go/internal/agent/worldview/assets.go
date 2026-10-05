package worldview

import "embed"

// The world view worker is a separate locked asset from the inference worker,
// so it gets its own runtime directory while sharing the agent's uv.
//
//go:embed worker.py pyproject.toml uv.lock
var assets embed.FS

var assetFiles = []string{"worker.py", "pyproject.toml", "uv.lock"}
