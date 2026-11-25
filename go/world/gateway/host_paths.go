package gateway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func validateHostStatePaths(paths []string, options Options) error {
	for _, boundary := range options.StateBoundaries {
		if boundary.Directory == "" || boundary.Subdirectory == "" || boundary.Subdirectory == "." || boundary.Subdirectory == ".." || strings.ContainsAny(boundary.Subdirectory, "/\\") {
			return errors.New("invalid host state boundary")
		}
		lexicalRoot, err := filepath.Abs(boundary.Directory)
		if err != nil {
			return err
		}
		root, err := filepath.EvalSymlinks(lexicalRoot)
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		allowed := filepath.Join(root, boundary.Subdirectory)
		var files []os.FileInfo
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := os.Stat(filepath.Join(root, entry.Name()))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			files = append(files, info)
		}
		for _, path := range paths {
			resolved, err := canonicalDiagnosticPath(path, 0)
			if err != nil {
				return err
			}
			lexical, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			// A spelling beneath the host store may not escape through a symlink.
			// An alias entering the host store is checked by its resolved location.
			if pathWithin(lexical, lexicalRoot) || pathWithin(lexical, root) || pathWithin(resolved, root) {
				if !pathWithin(resolved, allowed) {
					return fmt.Errorf("World state must use the host's %s subdirectory or a separate directory", boundary.Subdirectory)
				}
			}
			if pathWithin(root, resolved) {
				return errors.New("World state cannot own the host store or an ancestor")
			}
			info, err := os.Stat(resolved)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			for _, file := range files {
				if os.SameFile(info, file) {
					return errors.New("World state aliases an existing host file")
				}
			}
		}
	}
	return nil
}
