package go2

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Editable examples are separate from sources: changing a starter app must not
// invalidate the managed robot runtime or trigger a runtime rebuild.
// Keep individual build inputs explicit so tests and generated files stay out.
//
//go:embed examples/patrol/Dockerfile examples/patrol/.dockerignore examples/patrol/wendy.json examples/patrol/README.md
//go:embed examples/patrol/go2_io.py examples/patrol/patrol.py examples/patrol/ros_app.py
//go:embed examples/patrol/unitree_api/CMakeLists.txt examples/patrol/unitree_api/package.xml examples/patrol/unitree_api/LICENSE examples/patrol/unitree_api/UPSTREAM.md
//go:embed examples/patrol/unitree_api/msg/Request.msg examples/patrol/unitree_api/msg/RequestHeader.msg examples/patrol/unitree_api/msg/RequestIdentity.msg examples/patrol/unitree_api/msg/RequestLease.msg examples/patrol/unitree_api/msg/RequestPolicy.msg
//go:embed examples/roam/Dockerfile examples/roam/wendy.json examples/roam/README.md
//go:embed examples/roam/go2_io.py examples/roam/app.py examples/roam/controller.py examples/roam/ros_app.py examples/roam/index.html
//go:embed examples/roam/unitree_api/CMakeLists.txt examples/roam/unitree_api/package.xml examples/roam/unitree_api/LICENSE examples/roam/unitree_api/UPSTREAM.md
//go:embed examples/roam/unitree_api/msg/Request.msg examples/roam/unitree_api/msg/RequestHeader.msg examples/roam/unitree_api/msg/RequestIdentity.msg examples/roam/unitree_api/msg/RequestLease.msg examples/roam/unitree_api/msg/RequestPolicy.msg
//go:embed examples/teleop/Dockerfile examples/teleop/wendy.json examples/teleop/README.md
//go:embed examples/teleop/go2_io.py examples/teleop/teleop.py examples/teleop/index.html
//go:embed examples/teleop/unitree_api/CMakeLists.txt examples/teleop/unitree_api/package.xml examples/teleop/unitree_api/LICENSE examples/teleop/unitree_api/UPSTREAM.md
//go:embed examples/teleop/unitree_api/msg/Request.msg examples/teleop/unitree_api/msg/RequestHeader.msg examples/teleop/unitree_api/msg/RequestIdentity.msg examples/teleop/unitree_api/msg/RequestLease.msg examples/teleop/unitree_api/msg/RequestPolicy.msg
//go:embed examples/sensors/Dockerfile examples/sensors/.dockerignore examples/sensors/wendy.json examples/sensors/README.md
//go:embed examples/sensors/go2_io.py examples/sensors/dashboard.py examples/sensors/index.html
var exampleSources embed.FS

// MaterializeExample copies a complete standalone example into a new writable
// directory. It never replaces an existing directory or file. The caller owns
// the resulting project and can edit it independently of the embedded runtime.
func MaterializeExample(kind, targetDir string) (string, error) {
	switch kind {
	case "patrol", "roam", "teleop", "sensors":
	default:
		return "", fmt.Errorf("unknown Go2 example %q", kind)
	}
	if strings.TrimSpace(targetDir) == "" {
		return "", fmt.Errorf("Go2 example directory is empty")
	}
	target, err := filepath.Abs(targetDir)
	if err != nil {
		return "", err
	}
	tree, err := fs.Sub(exampleSources, "examples/"+kind)
	if err != nil {
		return "", err
	}
	files, err := treeFiles(tree)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		return "", fmt.Errorf("creating Go2 example directory: %w", err)
	}
	for _, name := range files {
		data, err := fs.ReadFile(tree, name)
		if err != nil {
			return "", err
		}
		path := filepath.Join(target, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return "", err
		}
		_, writeErr := file.Write(data)
		closeErr := file.Close()
		if writeErr != nil {
			return "", writeErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	return target, nil
}
