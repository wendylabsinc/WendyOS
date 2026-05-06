//go:build windows

package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// linkOrCopyDir makes the contents of src reachable from dst on Windows.
// os.Symlink requires Developer Mode or admin, which a typical end-user
// doesn't have, so we try in order:
//
//  1. os.Symlink (works in Developer Mode / admin)
//  2. mklink /J — directory junction, no privileges required for local NTFS
//  3. recursive copy — last-resort fallback for cross-volume targets, where
//     junctions are unsupported
func linkOrCopyDir(src, dst string) error {
	if err := os.Symlink(src, dst); err == nil {
		return nil
	}
	if err := makeJunction(src, dst); err == nil {
		return nil
	}
	if err := copyDir(src, dst); err != nil {
		return fmt.Errorf("link/junction/copy all failed for %s: %w", src, err)
	}
	return nil
}

// makeJunction creates an NTFS directory junction from dst to src using the
// built-in cmd.exe `mklink /J` command. Junctions are local-only reparse
// points and do not require elevated privileges.
func makeJunction(src, dst string) error {
	out, err := exec.Command("cmd.exe", "/C", "mklink", "/J", dst, src).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J %s %s: %w (%s)", dst, src, err, out)
	}
	return nil
}

func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !srcInfo.IsDir() {
		return errors.New("copyDir: source is not a directory")
	}
	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
