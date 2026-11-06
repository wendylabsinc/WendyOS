package jobops

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

//go:embed artifacts.py
var artifactsProgram string

// Pull downloads selected immutable file snapshots into a confined local root.
// Data never enters MCP results. Existing files are hashed before skipping them.
func Pull(ctx context.Context, conn *grpcclient.AgentConnection, jobPath, destination string, files []string) (map[string]any, error) {
	if !path.IsAbs(jobPath) || path.Clean(jobPath) == "/" || len(jobPath) > 4096 || strings.ContainsRune(jobPath, 0) {
		return nil, fmt.Errorf("job_path must be an absolute job directory")
	}
	if err := validateArtifactFiles(files); err != nil || destination == "" {
		return nil, fmt.Errorf("select one to 64 safe relative files and a destination")
	}
	if conn == nil || !conn.IsMTLS {
		return nil, fmt.Errorf("artifact retrieval requires an authenticated device connection")
	}
	seen := map[string]bool{}
	for _, name := range files {
		seen[name] = true
	}
	destination, err := filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(destination, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	known := map[string]string{}
	for _, name := range files {
		existing, statErr := root.Lstat(name)
		if statErr != nil || !existing.Mode().IsRegular() {
			continue
		}
		f, e := root.Open(name)
		if e != nil {
			continue
		}
		info, e := f.Stat()
		if e == nil && info.Mode().IsRegular() && info.Size() <= 16*1024*1024 {
			hash := sha256.New()
			if _, e = io.Copy(hash, io.LimitReader(f, 16*1024*1024+1)); e == nil {
				known[name] = hex.EncodeToString(hash.Sum(nil))
			}
		}
		f.Close()
	}
	args, _ := json.Marshal(map[string]any{"path": jobPath, "files": files, "known": known})
	wire, err := execute(ctx, conn, artifactsProgram, args, 24*1024*1024, 120*time.Second)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(wire)
	var response struct {
		Complete bool `json:"complete"`
		Files    []struct {
			Path      string `json:"path"`
			SHA       string `json:"sha256"`
			Bytes     int64  `json:"bytes"`
			Unchanged bool   `json:"unchanged"`
			Data      string `json:"data"`
		} `json:"files"`
	}
	if err = json.Unmarshal(raw, &response); err != nil || !response.Complete || len(response.Files) != len(files) {
		return nil, fmt.Errorf("incomplete artifact transfer")
	}
	stageBase := ".wendy-artifacts-" + rand.Text()
	if err = root.Mkdir(stageBase, 0700); err != nil {
		return nil, err
	}
	defer root.RemoveAll(stageBase)
	stage, err := root.OpenRoot(stageBase)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	rows := []map[string]any{}
	transferred := int64(0)
	skipped := 0
	for _, file := range response.Files {
		if !seen[file.Path] {
			return nil, fmt.Errorf("unexpected artifact path")
		}
		delete(seen, file.Path)
		if file.Unchanged {
			existing, e := root.Lstat(file.Path)
			if e != nil || !existing.Mode().IsRegular() {
				return nil, fmt.Errorf("unchanged local artifact is no longer regular")
			}
			current, e := root.Open(file.Path)
			if e != nil {
				return nil, e
			}
			hash := sha256.New()
			_, e = io.Copy(hash, io.LimitReader(current, 16*1024*1024+1))
			current.Close()
			if e != nil {
				return nil, e
			}
			if known[file.Path] != file.SHA || hex.EncodeToString(hash.Sum(nil)) != file.SHA {
				return nil, fmt.Errorf("unverified unchanged artifact")
			}
			skipped++
		} else {
			data, e := base64.StdEncoding.DecodeString(file.Data)
			hash := sha256.Sum256(data)
			if e != nil || int64(len(data)) != file.Bytes || hex.EncodeToString(hash[:]) != file.SHA {
				return nil, fmt.Errorf("artifact checksum mismatch: %s", file.Path)
			}
			if e = stage.MkdirAll(filepath.Dir(file.Path), 0700); e != nil {
				return nil, e
			}
			f, e := stage.OpenFile(file.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if e != nil {
				return nil, e
			}
			_, e = f.Write(data)
			closeErr := f.Close()
			if e != nil {
				return nil, e
			}
			if closeErr != nil {
				return nil, closeErr
			}
			transferred += file.Bytes
		}
		rows = append(rows, map[string]any{"path": file.Path, "sha256": file.SHA, "bytes": file.Bytes, "unchanged": file.Unchanged})
	}
	// Verify all snapshots before any destination file is replaced.
	for _, file := range response.Files {
		if file.Unchanged {
			continue
		}
		if err = root.MkdirAll(filepath.Dir(file.Path), 0700); err != nil {
			return nil, err
		}
		if err = root.Rename(filepath.Join(stageBase, file.Path), file.Path); err != nil {
			return nil, err
		}
	}
	source := conn.MeshHost
	if source == "" {
		source = conn.Addr
	}
	if source == "" {
		source = conn.Host
	}
	manifest := map[string]any{"complete": true, "job_path": jobPath, "source": source, "files": rows}
	encoded, _ := json.Marshal(manifest)
	name := ".wendy-artifacts-manifest.json"
	// Manifest is the completion marker and is published only after all files.
	f, err := root.OpenFile(filepath.Join(stageBase, "manifest.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.Write(encoded)
	e := f.Close()
	if err != nil {
		return nil, err
	}
	if e != nil {
		return nil, e
	}
	if err = root.Rename(filepath.Join(stageBase, "manifest.json"), name); err != nil {
		return nil, err
	}
	hash := sha256.Sum256(encoded)
	return map[string]any{"complete": true, "verified_files": len(rows), "unchanged_files": skipped, "transferred_bytes": transferred, "manifest_path": filepath.Join(destination, name), "manifest_sha256": hex.EncodeToString(hash[:])}, nil
}

func validateArtifactFiles(files []string) error {
	if len(files) < 1 || len(files) > 64 {
		return fmt.Errorf("select one to 64 files")
	}
	seen := map[string]bool{}
	for _, name := range files {
		if name == "." || strings.HasPrefix(strings.Split(name, "/")[0], ".wendy-artifacts-") || len(name) > 1024 || !filepath.IsLocal(name) || strings.Contains(name, "\\") || path.Clean(name) != name || seen[name] {
			return fmt.Errorf("files must be unique safe relative paths")
		}
		seen[name] = true
	}
	return nil
}
