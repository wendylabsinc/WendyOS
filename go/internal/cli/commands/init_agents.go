package commands

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
)

const (
	initAgentsFile = "AGENTS.md"
	initClaudeFile = "CLAUDE.md"

	// initClaudeMD imports AGENTS.md so Claude Code loads it even where it
	// reads only CLAUDE.md: versions before 2.1.277, and projects below a
	// directory that has its own CLAUDE.md.
	initClaudeMD = "@AGENTS.md\n"
)

// initAgentsTemplate is the AGENTS.md `wendy init` writes into new projects so
// coding agents (Codex, Cursor, Claude Code) start with the project's Wendy
// commands and a docs index. Agents follow a compact index better than a long
// one, so tests cap it at 8 KB and check every docs URL against the docs tree.
//
//go:embed init_agents.md.tmpl
var initAgentsTemplate string

var initAgentsTmpl = template.Must(template.New(initAgentsFile).Parse(initAgentsTemplate))

// initLanguageNames are display names for the language keys init and the
// templates registry use.
var initLanguageNames = map[string]string{
	langPython: "Python",
	langSwift:  "Swift",
	langRust:   "Rust",
	langNode:   "Node (TypeScript)",
	langCpp:    "C++",
	langMojo:   "Mojo",
	"c":        "C",
}

// initTutorialDirs maps a language key to its WendyOS tutorials directory
// under guides/tutorials in the docs.
var initTutorialDirs = map[string]string{
	langPython: "python",
	langSwift:  "swift",
	langRust:   "rust",
	langNode:   "typescript",
	langCpp:    "cpp",
	langMojo:   "mojo",
}

type initAgentsData struct {
	AppID        string
	LanguageName string
	Linux        bool
	Lite         bool
	Mac          bool
	// Tutorial is the docs path of the language's hello-world tutorial, or
	// empty when there is none for the target.
	Tutorial string
}

// renderInitAgentsMD renders the AGENTS.md for a project with the given app
// ID, init target and language key.
func renderInitAgentsMD(appID, target, language string) ([]byte, error) {
	data := initAgentsData{
		AppID:        appID,
		LanguageName: language,
		Linux:        target == targetWendyOS,
		Lite:         target == targetWendyLite,
		Mac:          target == targetDarwin,
	}
	if name, ok := initLanguageNames[language]; ok {
		data.LanguageName = name
	}
	if dir, ok := initTutorialDirs[language]; ok && data.Linux {
		data.Tutorial = "guides/tutorials/" + dir + "/hello-world/"
	}

	var buf bytes.Buffer
	if err := initAgentsTmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("rendering %s: %w", initAgentsFile, err)
	}
	return buf.Bytes(), nil
}

// writeInitAgentsFiles writes AGENTS.md into the project root and, when it
// did and the project has no CLAUDE.md, a CLAUDE.md that imports it. It never
// replaces an existing file: an AGENTS.md or CLAUDE.md the user or a template
// already has wins. Returns the names of the files it created.
func writeInitAgentsFiles(dir, appID, target, language string) ([]string, error) {
	content, err := renderInitAgentsMD(appID, target, language)
	if err != nil {
		return nil, err
	}

	created, err := createFileIfAbsent(filepath.Join(dir, initAgentsFile), content)
	if err != nil || !created {
		return nil, err
	}
	files := []string{initAgentsFile}

	created, err = createFileIfAbsent(filepath.Join(dir, initClaudeFile), []byte(initClaudeMD))
	if err != nil {
		return files, err
	}
	if created {
		files = append(files, initClaudeFile)
	}
	return files, nil
}

// createFileIfAbsent writes content to path unless something already exists
// there, reporting whether it created the file. A partial write is removed so
// a later init can create the file again.
func createFileIfAbsent(path string, content []byte) (bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("creating %s: %w", filepath.Base(path), err)
	}
	_, err = f.Write(content)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return false, fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	return true, nil
}

// scaffoldInitAgentsFiles runs writeInitAgentsFiles for the init flows. A
// failure is a warning, not an error: the project is already scaffolded, and
// a retry would stop at "wendy.json already exists".
func scaffoldInitAgentsFiles(dir, appID, target, language string) {
	files, err := writeInitAgentsFiles(dir, appID, target, language)
	if len(files) > 0 {
		cliSuccess("Created %s for AI coding agents", strings.Join(files, " and "))
	}
	if err != nil {
		cliNotice("  Warning: %v", err)
		return
	}
	// Claude Code reads an existing CLAUDE.md instead of AGENTS.md.
	if len(files) > 0 && !slices.Contains(files, initClaudeFile) {
		cliLogln("  CLAUDE.md already exists; add a line %q to it so Claude Code reads AGENTS.md too.", "@AGENTS.md")
	}
}
