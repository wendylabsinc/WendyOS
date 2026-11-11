package arduinotoolchain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultFQBN = "arduino:zephyr:unoq"
	// DefaultCore is the arduino:zephyr version used when sketch.yaml pins none.
	DefaultCore = "1.1.0"
)

// bridgeLibraries is what a sketch needs to talk to Linux: the core ships
// Arduino_RouterBridge only as a stub.
var bridgeLibraries = []any{
	"Arduino_RouterBridge (0.4.4)",
	map[string]any{"dependency": "Arduino_RPClite (0.3.1)"},
	map[string]any{"dependency": "ArxContainer (0.7.0)"},
	map[string]any{"dependency": "ArxTypeTraits (0.3.2)"},
	map[string]any{"dependency": "DebugLog (0.8.4)"},
	map[string]any{"dependency": "MsgPack (0.4.2)"},
}

// Prepared is the sketch as Wendy builds it.
type Prepared struct {
	// Dir is the sketch folder to compile: the project's own, or a staged copy
	// when sketch.yaml needed defaults. The project's files are never edited.
	Dir     string
	Profile string
	Core    string
	// Notices name each default applied, for the user.
	Notices []string
}

// PrepareSketch checks the sketch's profile and fills in what an Arduino App
// Lab sketch.yaml leaves out: the board, the core version and the Bridge
// libraries. A sketch.yaml that states all three is used as it is.
func PrepareSketch(sketchDir, profile string) (*Prepared, error) {
	path := filepath.Join(sketchDir, "sketch.yaml")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, _, err := loadProfile(sketchDir, profile)
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	notices := applySketchDefaults(doc, profile, sketchDir)
	dir := sketchDir
	if len(notices) > 0 {
		if dir, err = stageSketch(sketchDir, doc); err != nil {
			return nil, fmt.Errorf("preparing the sketch: %w", err)
		}
	}
	name, core, err := LoadProfile(dir, profile)
	if err != nil {
		return nil, err
	}
	return &Prepared{Dir: dir, Profile: name, Core: core, Notices: notices}, nil
}

// applySketchDefaults edits doc in place and returns one notice per default.
func applySketchDefaults(doc map[string]any, profile, sketchDir string) []string {
	profiles, _ := doc["profiles"].(map[string]any)
	name := profile
	if name == "" {
		name, _ = doc["default_profile"].(string)
	}
	if name == "" && len(profiles) == 1 {
		for only := range profiles {
			name = only
		}
	}
	p, ok := profiles[name].(map[string]any)
	if !ok {
		return nil
	}
	var notices []string
	if fqbn, _ := p["fqbn"].(string); fqbn == "" {
		p["fqbn"] = defaultFQBN
		notices = append(notices, "sketch.yaml names no board (fqbn); building for "+defaultFQBN)
	}
	pinned := "arduino:zephyr (" + DefaultCore + ")"
	platforms, _ := p["platforms"].([]any)
	if len(platforms) == 0 {
		p["platforms"] = []any{map[string]any{"platform": pinned}}
		notices = append(notices, "sketch.yaml lists no platform; using "+pinned)
	}
	for _, entry := range platforms {
		if m, ok := entry.(map[string]any); ok && m["platform"] == "arduino:zephyr" {
			m["platform"] = pinned
			notices = append(notices, "sketch.yaml does not pin arduino:zephyr; using "+pinned)
		}
	}
	libs, _ := p["libraries"].([]any)
	listed := map[string]bool{}
	for _, entry := range libs {
		listed[libraryName(entry)] = true
	}
	if !listed["Arduino_RouterBridge"] && sketchIncludes(sketchDir, "Arduino_RouterBridge.h") {
		for _, entry := range bridgeLibraries {
			if !listed[libraryName(entry)] {
				libs = append(libs, entry)
			}
		}
		p["libraries"] = libs
		notices = append(notices, "sketch.yaml does not list Arduino_RouterBridge; adding version 0.4.4 and its dependencies")
	}
	return notices
}

// libraryName returns the name in a profile's library entry: "Name (1.2.3)"
// or {dependency: "Name (1.2.3)"}.
func libraryName(entry any) string {
	if m, ok := entry.(map[string]any); ok {
		entry = m["dependency"]
	}
	text, _ := entry.(string)
	name, _, _ := strings.Cut(text, " (")
	return strings.TrimSpace(name)
}

func sketchIncludes(sketchDir, header string) bool {
	found := false
	_ = filepath.WalkDir(sketchDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found {
			return err
		}
		switch filepath.Ext(path) {
		case ".ino", ".cpp", ".h", ".hpp", ".c":
			if data, err := os.ReadFile(path); err == nil && bytes.Contains(data, []byte(header)) {
				found = true
			}
		}
		return nil
	})
	return found
}

// stageSketch mirrors the sketch into Wendy's cache with doc as its
// sketch.yaml. The folder keeps its name: arduino-cli requires it to match the
// main .ino file. Unchanged files are left alone, so the next compile stays
// incremental.
func stageSketch(sketchDir string, doc map[string]any) (string, error) {
	root, err := cacheDir()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(sketchDir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(abs))
	dst := filepath.Join(root, "arduino", "sketches", hex.EncodeToString(sum[:8]), filepath.Base(abs))
	yamlOut, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	keep := map[string]bool{"sketch.yaml": true}
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(abs, path)
		if d.IsDir() {
			if rel != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return mirrorDir(filepath.Join(dst, rel))
		}
		// Stat follows a symlinked source file; anything else is not sketch source.
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || rel == "sketch.yaml" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		keep[rel] = true
		return writeIfChanged(filepath.Join(dst, rel), data)
	})
	if err != nil {
		return "", err
	}
	if err := writeIfChanged(filepath.Join(dst, "sketch.yaml"), yamlOut); err != nil {
		return "", err
	}
	// Files deleted from the project must not linger in the copy.
	err = filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if rel, _ := filepath.Rel(dst, path); !keep[rel] {
			return os.Remove(path)
		}
		return nil
	})
	return dst, err
}

// mirrorDir makes path a directory, replacing a file that had its name.
func mirrorDir(path string) error {
	if info, err := os.Lstat(path); err == nil && !info.IsDir() {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return os.MkdirAll(path, 0o755)
}

// writeIfChanged writes data unless the file already holds it. A changed file
// gets a fresh modification time, so the compiler never takes it for built.
func writeIfChanged(path string, data []byte) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.IsDir():
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	case err == nil:
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
			return nil
		}
	}
	return os.WriteFile(path, data, 0o644)
}
