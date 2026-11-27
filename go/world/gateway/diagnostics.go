package gateway

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
)

type diagnosticsConfig struct {
	Path string `json:"path"`
}
type simulationFaults struct {
	ReplayCommitDelayMS int `json:"replay_commit_delay_ms"`
	TaskCommitDelayMS   int `json:"task_commit_delay_ms"`
}

// Every optional diagnostic destination is disjoint from protected files and
// from the other diagnostic/trigger namespaces, including their lock aliases.
// Validate before opening any exporter or durable store.
func validateDiagnosticPaths(metrics *diagnosticsConfig, trace *diagnostics.Config, protected []string) error {
	var paths []string
	if metrics != nil {
		paths = append(paths, metrics.Path)
	}
	if trace != nil {
		paths = append(paths, trace.Path, trace.TriggerPath)
	}
	for i, path := range paths {
		others := append([]string{}, protected...)
		for j, other := range paths {
			if i != j {
				others = append(others, other)
			}
		}
		if err := validateDiagnosticsPath(path, others); err != nil {
			return err
		}
	}
	return nil
}

func validateDiagnosticsPath(path string, protected []string) error {
	if path == "" {
		return errors.New("diagnostics path required")
	}
	a, err := canonicalDiagnosticPath(path, 0)
	if err != nil {
		return err
	}
	for _, p := range protected {
		// Atomic replacement of a locked sidecar would detach its flock from
		// the pathname and allow a second writer to lock a different inode.
		for _, candidate := range []string{p, p + ".lock"} {
			b, err := canonicalDiagnosticPath(candidate, 0)
			if err != nil {
				return err
			}
			if pathWithin(a, b) || pathWithin(b, a) {
				return errors.New("diagnostics must be separate from configuration, durable state and lock files")
			}
		}
	}
	return nil
}

func pathWithin(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

// Resolve existing ancestors and dangling final links before stores create
// their paths. This also handles aliases whose target directories do not exist.
func canonicalDiagnosticPath(path string, depth int) (string, error) {
	if depth > 40 {
		return "", errors.New("too many diagnostic path links")
	}
	// Cleaning '..' before following a symlink can name a different inode
	// from the original OS open. Keep this optional feature conservative.
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == ".." {
			return "", errors.New("diagnostics require paths without parent traversal")
		}
	}
	current, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return filepath.Join(append([]string{resolved}, suffix...)...), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		info, statErr := os.Lstat(current)
		if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(current)
			if readErr != nil {
				return "", readErr
			}
			for _, part := range strings.Split(target, string(filepath.Separator)) {
				if part == ".." {
					return "", errors.New("diagnostics require dangling link targets without parent traversal")
				}
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(current), target)
			}
			resolved, resolveErr := canonicalDiagnosticPath(target, depth+1)
			if resolveErr != nil {
				return "", resolveErr
			}
			return filepath.Join(append([]string{resolved}, suffix...)...), nil
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		current = parent
	}
}
