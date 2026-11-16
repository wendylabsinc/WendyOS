// windowsvmfreeze masks automatic Agent replacement in an owned test VM before
// its first boot. It edits only the pinned GRUB config in that VM's private ESP.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

// WendyOS-Builder tag 0.21.0: meta-vm-extensions/files/wic/vm-grubAB-aa64.cfg.
const originalSHA = "74a16f9b7967143b2879e94e089e36f97a4117094a6d0fe3b6be2c15eb090ac3"
const mask = "systemd.mask=wendyos-agent-updater.service"
const grubPath = "/EFI/BOOT/grub.cfg"

func freeze(fs filesystem.FileSystem, expected string) (map[string]string, error) {
	original, err := fs.ReadFile(grubPath)
	if err != nil {
		return nil, err
	}
	before := fmt.Sprintf("%x", sha256.Sum256(original))
	if before != expected {
		return nil, fmt.Errorf("guest GRUB config differs from the pinned release")
	}
	text := string(original)
	// All three boot entries, including manual A/B selection, use this suffix.
	boundary := "console=ttyAMA0,115200"
	if strings.Count(text, boundary) != 3 || strings.Contains(text, mask) {
		return nil, fmt.Errorf("unexpected guest boot entries")
	}
	patched := strings.ReplaceAll(text, boundary, boundary+" "+mask)
	file, err := fs.OpenFile(grubPath, os.O_RDWR|os.O_TRUNC)
	if err != nil {
		return nil, err
	}
	_, writeErr := file.Write([]byte(patched))
	closeErr := file.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	saved, err := fs.ReadFile(grubPath)
	if err != nil || string(saved) != patched {
		return nil, fmt.Errorf("guest boot mask readback failed: %v", err)
	}
	return map[string]string{"original_grub_sha256": before, "masked_grub_sha256": fmt.Sprintf("%x", sha256.Sum256(saved)), "kernel_argument": mask}, nil
}

func run() error {
	if runtime.GOOS != "windows" || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" || len(os.Args) != 3 {
		return fmt.Errorf("requires owned hosted Windows VM config directory and name")
	}
	root, err := filepath.EvalSymlinks(os.Args[1])
	if err != nil {
		return err
	}
	temporary, err := filepath.EvalSymlinks(os.Getenv("RUNNER_TEMP"))
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(temporary, root)
	if err != nil || relative == "." || strings.HasPrefix(relative, "..") || filepath.IsAbs(relative) {
		return fmt.Errorf("config must be private runner scratch")
	}
	if err := vm.ValidName(os.Args[2]); err != nil {
		return err
	}
	store := vm.Store{Root: filepath.Join(root, "vms")}
	path := store.DiskPath(os.Args[2])
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(resolved, path) {
		return fmt.Errorf("refusing linked VM disk path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("requires private regular VM disk")
	}
	// The fixture calls this before any vm start, and no state file may exist.
	if _, err := os.Stat(filepath.Join(store.Dir(os.Args[2]), "state.json")); !os.IsNotExist(err) {
		return fmt.Errorf("VM has already been started")
	}
	disk, err := diskfs.Open(path)
	if err != nil {
		return err
	}
	defer disk.Close()
	table, err := disk.GetPartitionTable()
	if err != nil {
		return err
	}
	g, ok := table.(*gpt.Table)
	if !ok {
		return fmt.Errorf("requires GPT guest")
	}
	esp := 0
	for i, p := range g.Partitions {
		if p.Type == gpt.EFISystemPartition {
			if esp != 0 {
				return fmt.Errorf("ambiguous ESP")
			}
			esp = i + 1
		}
	}
	if esp == 0 {
		return fmt.Errorf("missing ESP")
	}
	fs, err := disk.GetFilesystem(esp)
	if err != nil {
		return err
	}
	defer fs.Close()
	receipt, err := freeze(fs, originalSHA)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(receipt)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
