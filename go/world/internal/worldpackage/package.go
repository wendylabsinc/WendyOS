// Package worldpackage verifies content-addressed simulation package files.
// Installation neither compiles a model nor authorizes its execution.
package worldpackage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

const (
	SchemaV0      = "wendy.world.package.v0"
	SchemaV1      = "wendy.world.package.v1"
	MaxRootBytes  = 1_000_000
	MaxModelBytes = 8_000_000
	MaxFileBytes  = 8_000_000
	MaxTotalBytes = 64 << 20
	MaxFiles      = 256
)

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Manifest struct {
	SHA256        string `json:"sha256"`
	Site          string `json:"site"`
	Revision      string `json:"revision"`
	FrameRevision string `json:"frame_revision"`
	Files         []File `json:"files"`
}
type Bundle struct {
	Manifest Manifest          `json:"manifest"`
	Root     []byte            `json:"-"`
	Files    map[string][]byte `json:"-"`
}

func checksum(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func validDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == sha256.Size && strings.ToLower(s) == s
}
func required(fields map[string]json.RawMessage, key string) (json.RawMessage, error) {
	b, ok := fields[key]
	if !ok || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil, fmt.Errorf("package field %s required", key)
	}
	return b, nil
}
func text(fields map[string]json.RawMessage, key string) (string, error) {
	return textLimit(fields, key, 256)
}
func textLimit(fields map[string]json.RawMessage, key string, limit int) (string, error) {
	raw, e := required(fields, key)
	if e != nil {
		return "", e
	}
	var out string
	if e = json.Unmarshal(raw, &out); e != nil || out == "" || len(out) > limit || strings.TrimSpace(out) != out {
		return "", fmt.Errorf("invalid package %s", key)
	}
	return out, nil
}

// SafePath accepts portable slash-separated asset paths. It deliberately
// rejects names with ambiguous case/device/path semantics on supported hosts.
func SafePath(name string) error {
	if name == "" || len(name) > 1024 || strings.Contains(name, "\\") || path.IsAbs(name) || path.Clean(name) != name {
		return errors.New("invalid package file path")
	}
	parts := strings.Split(name, "/")
	if len(parts) > 32 {
		return errors.New("package path too deep")
	}
	for _, part := range parts {
		if len(part) == 0 || len(part) > 128 || part[0] == '.' || part[len(part)-1] == '.' {
			return errors.New("invalid package path segment")
		}
		for _, ch := range part {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-' || ch == '.') {
				return errors.New("nonportable package path")
			}
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
			return errors.New("reserved package path segment")
		}
	}
	return nil
}
func checkPaths(files []File) error {
	names := map[string]bool{"world.json": true}
	spellings := map[string]string{"world.json": "world.json"}
	for _, file := range files {
		if e := SafePath(file.Path); e != nil {
			return e
		}
		name := strings.ToLower(file.Path)
		if names[name] {
			return errors.New("duplicate or root-colliding package path")
		}
		names[name] = true
		parts := strings.Split(file.Path, "/")
		for i := range parts {
			prefix := strings.Join(parts[:i+1], "/")
			folded := strings.ToLower(prefix)
			if previous, ok := spellings[folded]; ok && previous != prefix {
				return errors.New("package path has inconsistent directory casing")
			}
			spellings[folded] = prefix
		}
	}
	for name := range names {
		for p := path.Dir(name); p != "."; p = path.Dir(p) {
			if names[p] {
				return errors.New("package file collides with parent directory")
			}
		}
	}
	return nil
}

// Inspect preserves the original root bytes' digest; it does not normalize or
// rewrite provenance, physical properties, licensing or application metadata.
// Root MJCF Size is zero until its bytes are verified. Neither supported schema
// declares its size. Version 1 validates the generic simulation envelope, while
// version 0 retains its existing application-metadata compatibility.
func Inspect(raw []byte) (Manifest, error) {
	var out Manifest
	if len(raw) < 1 || len(raw) > MaxRootBytes {
		return out, errors.New("package root outside size bound")
	}
	var fields map[string]json.RawMessage
	if e := authority.DecodeStrictLimit(raw, &fields, MaxRootBytes); e != nil {
		return out, e
	}
	// Reject numeric values that overflow the double-precision runtime even in
	// application-owned metadata. Standard JSON has no NaN/Infinity literals.
	var finiteJSON any
	if e := json.Unmarshal(raw, &finiteJSON); e != nil {
		return out, e
	}
	schema, e := text(fields, "schema_version")
	if e != nil || schema != SchemaV0 && schema != SchemaV1 {
		return out, errors.New("unsupported world package schema")
	}
	if schema == SchemaV1 {
		if e = validateGeneric(raw, fields); e != nil {
			return out, e
		}
	}
	origin, e := text(fields, "provenance")
	if e != nil || origin != "synthetic" {
		return out, errors.New("package transfer currently accepts synthetic provenance only")
	}
	if out.Site, e = text(fields, "site_id"); e != nil {
		return out, e
	}
	if out.Revision, e = text(fields, "package_revision"); e != nil {
		return out, e
	}
	if out.FrameRevision, e = text(fields, "frame_revision"); e != nil {
		return out, e
	}
	modelRaw, e := required(fields, "mujoco")
	if e != nil {
		return out, e
	}
	var model map[string]json.RawMessage
	if e = json.Unmarshal(modelRaw, &model); e != nil {
		return out, e
	}
	modelPath, e := textLimit(model, "path", 1024)
	if e != nil {
		return out, e
	}
	modelHash, e := text(model, "sha256")
	if e != nil || !validDigest(modelHash) {
		return out, errors.New("invalid model digest")
	}
	out.Files = []File{{Path: modelPath, SHA256: modelHash}}
	total := int64(len(raw))
	if assetRaw, ok := fields["assets"]; ok {
		if bytes.Equal(bytes.TrimSpace(assetRaw), []byte("null")) {
			return out, errors.New("null assets inventory")
		}
		var assets []json.RawMessage
		if e = json.Unmarshal(assetRaw, &assets); e != nil {
			return out, e
		}
		if len(assets) > MaxFiles-1 {
			return out, errors.New("too many package files")
		}
		extra := make([]File, 0, len(assets))
		for _, rawAsset := range assets {
			var a map[string]json.RawMessage
			if e = json.Unmarshal(rawAsset, &a); e != nil {
				return out, e
			}
			if len(a) != 3 {
				return out, errors.New("asset requires exactly path, sha256 and size")
			}
			var f File
			if f.Path, e = textLimit(a, "path", 1024); e != nil {
				return out, e
			}
			if f.SHA256, e = text(a, "sha256"); e != nil || !validDigest(f.SHA256) {
				return out, errors.New("invalid asset digest")
			}
			sizeRaw, e := required(a, "size")
			if e != nil {
				return out, e
			}
			if e = json.Unmarshal(sizeRaw, &f.Size); e != nil || f.Size < 1 || f.Size > MaxFileBytes {
				return out, errors.New("invalid asset size")
			}
			total += f.Size
			if total > MaxTotalBytes {
				return out, errors.New("package exceeds total size bound")
			}
			extra = append(extra, f)
		}
		sort.Slice(extra, func(i, j int) bool { return extra[i].Path < extra[j].Path })
		out.Files = append(out.Files, extra...)
	}
	if e = checkPaths(out.Files); e != nil {
		return out, e
	}
	out.SHA256 = checksum(raw)
	return out, nil
}

// Verify returns owned bytes. Callers must not mutate inputs concurrently with
// this call; returned metadata and buffers share no mutable input storage.
func Verify(raw []byte, blobs map[string][]byte) (Bundle, error) {
	root := append([]byte(nil), raw...)
	manifest, e := Inspect(root)
	if e != nil {
		return Bundle{}, e
	}
	if len(blobs) != len(manifest.Files) {
		return Bundle{}, errors.New("package files missing or extra")
	}
	out := Bundle{Manifest: manifest, Root: root, Files: make(map[string][]byte, len(blobs))}
	total := int64(len(root))
	for i, file := range manifest.Files {
		data, ok := blobs[file.Path]
		if !ok {
			return Bundle{}, fmt.Errorf("package file %s missing", file.Path)
		}
		if len(data) < 1 || len(data) > MaxFileBytes || file.Size != 0 && int64(len(data)) != file.Size {
			return Bundle{}, fmt.Errorf("package file %s has invalid size", file.Path)
		}
		owned := append([]byte(nil), data...)
		if checksum(owned) != file.SHA256 {
			return Bundle{}, fmt.Errorf("package file %s digest mismatch", file.Path)
		}
		total += int64(len(owned))
		if total > MaxTotalBytes {
			return Bundle{}, errors.New("package exceeds total size bound")
		}
		out.Manifest.Files[i].Size = int64(len(owned))
		out.Files[file.Path] = owned
	}
	return out, nil
}

func readRootFile(root *os.Root, name string, limit int64) ([]byte, error) {
	// Refuse stable symbolic aliases at every component; OpenRoot confines any
	// raced resolution to this tree, and O_NOFOLLOW protects the final entry.
	parts := strings.Split(name, "/")
	for i := range parts {
		st, e := root.Lstat(strings.Join(parts[:i+1], "/"))
		if e != nil {
			return nil, e
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("package input cannot contain symlinks")
		}
		if i < len(parts)-1 && !st.IsDir() {
			return nil, errors.New("package parent is not a directory")
		}
		if i == len(parts)-1 && (!st.Mode().IsRegular() || st.Size() < 1 || st.Size() > limit) {
			return nil, errors.New("package input must be a bounded regular file")
		}
	}
	f, e := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() < 1 || st.Size() > limit {
		return nil, errors.New("package input must be a bounded regular file")
	}
	data, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(data)) > limit {
		return nil, errors.New("package file exceeds bound")
	}
	return data, nil
}
func Load(name string) (Bundle, error) {
	absolute, e := filepath.Abs(name)
	if e != nil {
		return Bundle{}, e
	}
	root, e := os.OpenRoot(filepath.Dir(absolute))
	if e != nil {
		return Bundle{}, e
	}
	defer root.Close()
	raw, e := readRootFile(root, filepath.Base(absolute), MaxRootBytes)
	if e != nil {
		return Bundle{}, e
	}
	manifest, e := Inspect(raw)
	if e != nil {
		return Bundle{}, e
	}
	blobs := make(map[string][]byte, len(manifest.Files))
	var total int64 = int64(len(raw))
	for _, file := range manifest.Files {
		limit := int64(MaxFileBytes)
		if file.Size > 0 {
			limit = file.Size
		}
		if remaining := int64(MaxTotalBytes) - total; remaining < limit {
			limit = remaining
		}
		if limit < 1 {
			return Bundle{}, errors.New("package exceeds total bound")
		}
		data, e := readRootFile(root, file.Path, limit)
		if e != nil {
			return Bundle{}, fmt.Errorf("package file %s: %w", file.Path, e)
		}
		blobs[file.Path] = data
		total += int64(len(data))
	}
	return Verify(raw, blobs)
}
