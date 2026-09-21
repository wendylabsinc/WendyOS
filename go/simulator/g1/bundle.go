// Package g1 provides the authored source payload for the virtual G1 runtime.
// Large upstream assets are acquired from the embedded, pinned lockfiles during
// the container build; they are deliberately not part of the CLI binary.
package g1

import (
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
)

// Keep this allowlist narrow: embedding whole directories would accidentally
// ship downloaded meshes, Python environments, or generated ROS build output.
//
//go:embed g1_sim/*.py g1_sim/index.html g1_sim/viewer.js
//go:embed g1_sim/vendor/three.module.js g1_sim/vendor/three.core.js g1_sim/vendor/OrbitControls.js
//go:embed ros_ws/src/g1_command_ingress/src/command_ingress.cpp
//go:embed ros_ws/src/g1_command_ingress/CMakeLists.txt ros_ws/src/g1_command_ingress/package.xml
//go:embed assets.lock.json unitree.lock.json requirements*.txt
//go:embed tools/fetch_assets.py tools/fetch_unitree.py tools/prepare_unitree.py
//go:embed licenses/*.LICENSE UPSTREAM.md compatibility.json
//go:embed entrypoint.sh cyclonedds.xml Dockerfile Dockerfile.ros .dockerignore wendy.json
var sources embed.FS

var sourceDigest = sync.OnceValue(func() string {
	digest, err := digestTree(sources)
	if err != nil {
		// embed.FS is immutable and compiled into the program. Failure here is a
		// malformed source bundle, never a recoverable cache or device failure.
		panic(fmt.Sprintf("invalid embedded G1 source bundle: %v", err))
	}
	return digest
})

// SourceDigest identifies the exact embedded build inputs as sha256:<hex>.
// The canonical format hashes a version tag followed by lexically sorted
// slash-separated paths and their contents, each prefixed by its uint64
// big-endian byte length. File timestamps and host filesystem modes are ignored.
func SourceDigest() string { return sourceDigest() }

// Materialize returns an absolute, content-addressed container build directory
// below cacheRoot/g1. The published tree is read-only and must not be edited.
// Existing trees must match exactly; corrupt, incomplete, or user-modified
// caches are reported without replacing or deleting them. Concurrent CLI
// processes share a cache lock and publish only complete trees by rename.
func Materialize(cacheRoot string) (string, error) {
	return materialize(cacheRoot, sources, SourceDigest())
}

func materialize(cacheRoot string, tree fs.FS, digest string) (string, error) {
	if strings.TrimSpace(cacheRoot) == "" {
		return "", errors.New("G1 source cache root is empty")
	}
	root, err := filepath.Abs(cacheRoot)
	if err != nil {
		return "", fmt.Errorf("resolving G1 source cache: %w", err)
	}
	root = filepath.Join(root, "g1")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("creating G1 source cache: %w", err)
	}
	if err := requireDirectory(root); err != nil {
		return "", err
	}
	unlock, err := lockCache(root)
	if err != nil {
		return "", err
	}
	defer unlock()

	target := filepath.Join(root, strings.TrimPrefix(digest, "sha256:"))
	if _, err := os.Lstat(target); err == nil {
		if err := verifyCache(target, digest); err != nil {
			return "", err
		}
		return target, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspecting G1 source cache: %w", err)
	}

	temporary, err := os.MkdirTemp(root, ".materialize-")
	if err != nil {
		return "", fmt.Errorf("creating temporary G1 source directory: %w", err)
	}
	// Only this invocation's temporary tree is ever removed, including after a
	// failed read, write, chmod, or rename. Published/foreign trees stay intact.
	defer func() {
		if temporary != "" {
			removeTemporary(temporary)
		}
	}()
	if err := writeSources(tree, temporary); err != nil {
		return "", fmt.Errorf("materializing G1 sources: %w", err)
	}
	if err := verifyCache(temporary, digest); err != nil {
		return "", err
	}
	if err := makeReadOnly(temporary); err != nil {
		return "", fmt.Errorf("protecting G1 source cache: %w", err)
	}
	// macOS requires write permission on a directory to rename it. Keep only
	// this invocation's root writable until publication; its payload remains
	// read-only, and the cache lock excludes other cooperating readers.
	if err := os.Chmod(temporary, 0o700); err != nil {
		return "", fmt.Errorf("preparing G1 source cache publication: %w", err)
	}
	// Check again rather than replacing a directory created by a non-cooperating
	// cache writer while the payload was being prepared.
	if _, err := os.Lstat(target); err == nil {
		if err := verifyCache(target, digest); err != nil {
			return "", err
		}
		return target, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspecting G1 source cache: %w", err)
	}
	if err := os.Rename(temporary, target); err != nil {
		return "", fmt.Errorf("publishing G1 source cache: %w", err)
	}
	// Retain cleanup ownership until the published root is protected too.
	temporary = target
	if err := os.Chmod(target, 0o555); err != nil {
		return "", fmt.Errorf("protecting published G1 source cache: %w", err)
	}
	temporary = "" // Ownership moved to the published cache; never clean it up.
	return target, nil
}

func digestTree(tree fs.FS) (string, error) {
	files, err := treeFiles(tree)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte("wendy-g1-source-v1\x00"))
	var length [8]byte
	for _, name := range files {
		data, err := fs.ReadFile(tree, name)
		if err != nil {
			return "", err
		}
		for _, value := range [][]byte{[]byte(name), data} {
			binary.BigEndian.PutUint64(length[:], uint64(len(value)))
			h.Write(length[:])
			h.Write(value)
		}
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}

func treeFiles(tree fs.FS) ([]string, error) {
	var files []string
	err := fs.WalkDir(tree, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// Empty directories have no source semantics. Reject them so adding
			// an unexpected empty directory cannot pass cache verification.
			children, err := fs.ReadDir(tree, name)
			if err != nil {
				return err
			}
			if len(children) == 0 {
				return fmt.Errorf("source directory %q is empty", name)
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("source entry %q is not a regular file", name)
		}
		files = append(files, name)
		return nil
	})
	sort.Strings(files)
	return files, err
}

func requireDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("G1 source cache is not a directory: %s", path)
	}
	return nil
}

func verifyCache(path, expected string) error {
	if err := requireDirectory(path); err != nil {
		return fmt.Errorf("invalid G1 source cache %q: %w", path, err)
	}
	digest, err := digestTree(os.DirFS(path))
	if err != nil {
		return fmt.Errorf("invalid G1 source cache %q: %w", path, err)
	}
	if digest != expected {
		return fmt.Errorf("G1 source cache %q was modified: expected %s, got %s", path, expected, digest)
	}
	return nil
}

func writeSources(tree fs.FS, root string) error {
	files, err := treeFiles(tree)
	if err != nil {
		return err
	}
	for _, name := range files {
		data, err := fs.ReadFile(tree, name)
		if err != nil {
			return err
		}
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func makeReadOnly(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o444)
		if entry.IsDir() || entry.Name() == "entrypoint.sh" {
			mode = 0o555
		}
		return os.Chmod(path, mode)
	})
}

func removeTemporary(root string) {
	// Published trees can be read-only already if the final rename failed.
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil {
			if entry.IsDir() {
				return os.Chmod(path, 0o700)
			}
			if entry.Type().IsRegular() {
				return os.Chmod(path, 0o600)
			}
		}
		return err
	})
	_ = os.RemoveAll(root)
}

func lockCache(root string) (func(), error) {
	path := filepath.Join(root, ".materialize.lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("G1 cache lock is not a regular file: %s", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening G1 cache lock: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		locked, err := flock.TryLock(f)
		if err != nil || time.Now().After(deadline) {
			f.Close()
			if err != nil {
				return nil, fmt.Errorf("locking G1 source cache: %w", err)
			}
			return nil, errors.New("timed out waiting for G1 source cache lock")
		}
		if locked {
			return func() { _ = flock.Unlock(f); _ = f.Close() }, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}
