package espidftoolchain

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const componentManifest = "main/idf_component.yml"

// WendyCoreVersionSetup pins the wendy_core dependency of
// main/idf_component.yml to a Wendy Lite version. It is prepared before the
// user confirms it.
type WendyCoreVersionSetup struct {
	manifestPath string
	// Current is the version in the manifest, "" when it has none: the
	// default branch of the repository.
	Current string
	// Version is the version that Apply writes: the git ref of the Wendy
	// Lite version (WendyLiteGitRef).
	Version           string
	original, updated []byte
}

// PrepareWendyCoreVersion checks that the wendy_core dependency of
// main/idf_component.yml has version, the Wendy Lite version of the device.
// For a git dependency, the version is a git ref: the WendyLiteGitRef of
// version. It returns nil when there is nothing to change: the manifest does
// not declare wendy_core, or the dependency has that ref (SameVersion).
// Otherwise it prepares the change of that one line, without writing it, so
// that the comments and the layout of the manifest stay.
//
// Only a git dependency on the wendy-lite repository is checked. Another
// source (a local path, override_path, another repository, the registry)
// is a developer setup: unchecked then tells where wendy_core comes from.
// An empty version checks nothing.
func PrepareWendyCoreVersion(projectPath, version string) (setup *WendyCoreVersionSetup, unchecked string, err error) {
	if version == "" {
		return nil, "", nil
	}
	version = WendyLiteGitRef(version)
	path := filepath.Join(projectPath, componentManifest)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, "", fmt.Errorf("reading %s: %w", componentManifest, err)
	}
	dep := findWendyCoreDependency(&doc)
	if dep == nil {
		return nil, "", nil
	}
	if dep.Kind != yaml.MappingNode {
		return nil, "wendy_core comes from the component registry", nil
	}
	fields := map[string]*yaml.Node{}
	var keys []*yaml.Node
	for i := 0; i+1 < len(dep.Content); i += 2 {
		fields[dep.Content[i].Value] = dep.Content[i+1]
		keys = append(keys, dep.Content[i])
	}
	text := func(name string) string {
		if n := fields[name]; n != nil && n.Kind == yaml.ScalarNode {
			return n.Value
		}
		return ""
	}
	git := text("git")
	switch {
	case text("override_path") != "":
		return nil, "wendy_core comes from override_path " + text("override_path"), nil
	case git == "" && text("path") != "":
		return nil, "wendy_core comes from the local path " + text("path"), nil
	case git == "":
		return nil, "wendy_core comes from the component registry", nil
	case !isWendyLiteGitURL(git):
		return nil, "wendy_core comes from " + git, nil
	}
	current := text("version")
	if SameVersion(current, version) {
		return nil, "", nil
	}

	manual := fmt.Errorf("set version: %q in the wendy_core dependency of %s by hand", version, componentManifest)
	if dep.Style&yaml.FlowStyle != 0 {
		return nil, "", manual
	}
	lines := strings.Split(string(data), "\n")
	value := `"` + version + `"`
	if versionNode := fields["version"]; versionNode != nil {
		key := keyNode(keys, "version")
		if versionNode.Kind != yaml.ScalarNode || key.Line < 1 || key.Line > len(lines) {
			return nil, "", manual
		}
		line := lines[key.Line-1]
		cr := strings.HasSuffix(line, "\r")
		line = strings.TrimSuffix(line, "\r")
		switch {
		case versionNode.Tag == "!!null" && versionNode.Value == "":
			// "version:" with no value: the line is the key alone.
			line = line[:byteOffset(line, key.Column)] + "version: " + value
		case versionNode.Line == key.Line && versionNode.Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0:
			start := byteOffset(line, versionNode.Column)
			end := scalarEnd(line, start, versionNode)
			if end < 0 {
				return nil, "", manual
			}
			line = line[:start] + value + line[end:]
		default:
			return nil, "", manual
		}
		if cr {
			line += "\r"
		}
		lines[key.Line-1] = line
	} else {
		key := keyNode(keys, "git")
		if key == nil || fields["git"].Line != key.Line || key.Line < 1 || key.Line > len(lines) {
			return nil, "", manual
		}
		line := lines[key.Line-1]
		indent := line[:byteOffset(line, key.Column)]
		if strings.TrimLeft(indent, " \t") != "" {
			return nil, "", manual
		}
		added := indent + "version: " + value
		if strings.HasSuffix(line, "\r") {
			added += "\r"
		}
		lines = append(lines[:key.Line], append([]string{added}, lines[key.Line:]...)...)
	}
	return &WendyCoreVersionSetup{
		manifestPath: path,
		Current:      current,
		Version:      version,
		original:     data,
		updated:      []byte(strings.Join(lines, "\n")),
	}, "", nil
}

// Apply writes the pinned version. It refuses to write when the manifest
// changed since PrepareWendyCoreVersion. No idf.py run is needed: ESP-IDF
// configures the project again when the manifest changes, and the component
// manager then fetches the version.
func (s *WendyCoreVersionSetup) Apply() error {
	current, err := os.ReadFile(s.manifestPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, s.original) {
		return fmt.Errorf("%s changed during setup; run 'wendy run' again", componentManifest)
	}
	if err := os.WriteFile(s.manifestPath, s.updated, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", componentManifest, err)
	}
	return nil
}

// findWendyCoreDependency returns the value of the wendy_core (or
// <namespace>/wendy_core) entry of the dependencies map, or nil.
func findWendyCoreDependency(doc *yaml.Node) *yaml.Node {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "dependencies" || root.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		deps := root.Content[i+1]
		for j := 0; j+1 < len(deps.Content); j += 2 {
			name := deps.Content[j].Value
			if name == "wendy_core" || strings.HasSuffix(name, "/wendy_core") {
				return deps.Content[j+1]
			}
		}
	}
	return nil
}

func keyNode(keys []*yaml.Node, name string) *yaml.Node {
	for _, k := range keys {
		if k.Value == name {
			return k
		}
	}
	return nil
}

// byteOffset returns the byte offset of a 1-based YAML column (in
// characters) in line.
func byteOffset(line string, column int) int {
	offset := 0
	for i := 1; i < column && offset < len(line); i++ {
		_, size := utf8.DecodeRuneInString(line[offset:])
		offset += size
	}
	return offset
}

// scalarEnd returns the byte offset after the one-line scalar that starts
// at start in line, or -1 when it does not end on the line.
func scalarEnd(line string, start int, n *yaml.Node) int {
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		for i := start + 1; i < len(line); i++ {
			switch line[i] {
			case '\\':
				i++
			case '"':
				return i + 1
			}
		}
		return -1
	case n.Style&yaml.SingleQuotedStyle != 0:
		for i := start + 1; i < len(line); i++ {
			if line[i] == '\'' {
				if i+1 < len(line) && line[i+1] == '\'' {
					i++
					continue
				}
				return i + 1
			}
		}
		return -1
	}
	// A one-line plain scalar is its own text.
	if !strings.HasPrefix(line[start:], n.Value) {
		return -1
	}
	return start + len(n.Value)
}

// isWendyLiteGitURL reports whether url is the wendy-lite repository on
// GitHub, over HTTPS or SSH, with or without .git.
func isWendyLiteGitURL(url string) bool {
	url = strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(url)), "/"), ".git")
	for _, repo := range []string{"github.com/wendylabsinc/wendy-lite", "github.com:wendylabsinc/wendy-lite"} {
		if url == repo {
			return true
		}
		if rest, ok := strings.CutSuffix(url, repo); ok && strings.ContainsAny(rest[len(rest)-1:], "/@") {
			return true
		}
	}
	return false
}

// SameVersion reports whether two Wendy Lite versions are the same: equal
// texts, or the same commit given as a full and a short SHA (at least 7
// hex digits).
func SameVersion(a, b string) bool {
	if a == b {
		return true
	}
	a, b = strings.ToLower(a), strings.ToLower(b)
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 7 && isHex(a) && isHex(b) && strings.HasPrefix(b, a)
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
