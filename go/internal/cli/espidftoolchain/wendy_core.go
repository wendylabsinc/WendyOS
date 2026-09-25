package espidftoolchain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const wendyCoreGitURL = "https://github.com/wendylabsinc/wendy-lite.git"

// HasBuildComponent checks ESP-IDF's generated list of linked components.
func HasBuildComponent(projectPath, name string) (bool, error) {
	path := filepath.Join(projectPath, "build", "project_description.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading ESP-IDF project description: %w", err)
	}
	var description struct {
		Components []string `json:"build_components"`
	}
	if err := json.Unmarshal(data, &description); err != nil {
		return false, fmt.Errorf("reading ESP-IDF project description: %w", err)
	}
	for _, component := range description.Components {
		if component == name || strings.HasSuffix(component, "__"+name) {
			return true, nil
		}
	}
	return false, nil
}

// WendyCoreSetup holds source changes prepared before the user confirms setup.
type WendyCoreSetup struct {
	projectPath   string
	sourcePath    string
	original      []byte
	updated       []byte
	hasDependency bool
}

// SourcePath is relative to the project root, for display in the setup prompt.
func (s *WendyCoreSetup) SourcePath() string {
	path, _ := filepath.Rel(s.projectPath, s.sourcePath)
	return path
}

// PrepareWendyCore locates a single app_main in the main component and prepares
// its initialization without writing files. Unrecognized layouts need manual
// setup so we do not guess which entry point to change.
func PrepareWendyCore(projectPath string) (*WendyCoreSetup, error) {
	s := &WendyCoreSetup{projectPath: projectPath}
	manifestPath := filepath.Join(projectPath, "main", "idf_component.yml")
	data, err := os.ReadFile(manifestPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(data) != 0 {
		var manifest struct {
			Dependencies map[string]any `yaml:"dependencies"`
		}
		if err := yaml.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("reading %s: %w", manifestPath, err)
		}
		for name := range manifest.Dependencies {
			if name == "wendy_core" || strings.HasSuffix(name, "/wendy_core") {
				s.hasDependency = true
			}
		}
	}

	err = filepath.WalkDir(filepath.Join(projectPath, "main"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		switch filepath.Ext(path) {
		case ".c", ".cc", ".cpp", ".cxx":
		default:
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		matches := appMainDefinition.FindAllIndex(maskCCommentsAndStrings(data), -1)
		if len(matches) == 0 {
			return nil
		}
		if len(matches) != 1 || s.sourcePath != "" {
			return fmt.Errorf("found multiple app_main() definitions; add wendy_core and its initialization manually")
		}
		s.sourcePath = path
		s.original = data
		s.updated, err = initializeWendyCore(data, matches[0][1])
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("preparing wendy_core setup: %w", err)
	}
	if s.sourcePath == "" {
		return nil, fmt.Errorf("could not find app_main() in main/; add wendy_core and call ESP_ERROR_CHECK(wendy_core_init()) first in app_main() manually")
	}
	return s, nil
}

// Apply adds the managed dependency, writes the prepared initialization, and
// regenerates sdkconfig. ESP-IDF's component manager adds the manifest dependency
// to CMake's requirements automatically, so CMakeLists.txt needs no edit.
func (s *WendyCoreSetup) Apply(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.checkSource(); err != nil {
		return err
	}
	if !s.hasDependency {
		if err := s.runIDF(ctx, "add-dependency", "--component", "main", "--git", wendyCoreGitURL, "--git-path", "components/wendy_core", "wendy_core"); err != nil {
			return err
		}
	}
	if !bytes.Equal(s.original, s.updated) {
		// Adding a dependency may take time. Preserve edits made while it ran.
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.checkSource(); err != nil {
			return err
		}
		if err := os.WriteFile(s.sourcePath, s.updated, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", s.SourcePath(), err)
		}
	}
	return s.runIDF(ctx, "reconfigure")
}

func (s *WendyCoreSetup) checkSource() error {
	current, err := os.ReadFile(s.sourcePath)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, s.original) {
		return fmt.Errorf("%s changed during setup; run 'wendy run' again", s.SourcePath())
	}
	return nil
}

func (s *WendyCoreSetup) runIDF(ctx context.Context, args ...string) error {
	cmd := IdfCommandContext(ctx, args...)
	cmd.Dir = s.projectPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("idf.py %s: %w", args[0], err)
	}
	return nil
}

var (
	appMainDefinition   = regexp.MustCompile(`\bvoid\s+app_main\s*\(\s*(?:void\s*)?\)\s*\{`)
	cCommentsAndStrings = regexp.MustCompile(`(?s)/\*.*?\*/|//[^\r\n]*|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'`)
	coreInitCall        = regexp.MustCompile(`\bwendy_core_init\s*\(`)
	coreInitFirst       = regexp.MustCompile(`^\s*(?:ESP_ERROR_CHECK\s*\(\s*)?wendy_core_init\s*\(\s*\)\s*\)?\s*;`)
)

// Keep byte offsets and newlines intact while ignoring comments and literals.
func maskCCommentsAndStrings(data []byte) []byte {
	masked := bytes.Clone(data)
	for _, match := range cCommentsAndStrings.FindAllIndex(data, -1) {
		for i := match[0]; i < match[1]; i++ {
			if masked[i] != '\n' && masked[i] != '\r' {
				masked[i] = ' '
			}
		}
	}
	return masked
}

func initializeWendyCore(data []byte, bodyStart int) ([]byte, error) {
	masked := maskCCommentsAndStrings(data)
	depth, bodyEnd := 1, bodyStart
	for ; bodyEnd < len(masked) && depth > 0; bodyEnd++ {
		switch masked[bodyEnd] {
		case '{':
			depth++
		case '}':
			depth--
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("could not identify the end of app_main(); add wendy_core initialization manually")
	}
	body := masked[bodyStart : bodyEnd-1]
	newline := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		newline = "\r\n"
	}
	updated := bytes.Clone(data)
	if coreInitCall.Match(body) {
		if !coreInitFirst.Match(body) {
			return nil, fmt.Errorf("move the existing wendy_core_init() call to the first statement of app_main()")
		}
	} else {
		initialization := newline + "    ESP_ERROR_CHECK(wendy_core_init());" + newline
		updated = []byte(string(data[:bodyStart]) + initialization + string(data[bodyStart:]))
	}
	// Check include directives outside comments. Strings are masked, so match
	// their positions in the original source after locating an active directive.
	include := regexp.MustCompile(`(?m)^[\t ]*#[\t ]*include\b[^\r\n]*`)
	hasHeader := false
	for _, match := range include.FindAllIndex(masked, -1) {
		line := string(data[match[0]:match[1]])
		if strings.Contains(line, `"wendy_core.h"`) || strings.Contains(line, "<wendy_core.h>") {
			hasHeader = true
		}
	}
	if !hasHeader {
		updated = []byte("#include \"wendy_core.h\"" + newline + newline + string(updated))
	}
	return updated, nil
}
