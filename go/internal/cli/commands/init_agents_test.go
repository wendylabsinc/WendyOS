package commands

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// initAgentsMaxBytes is the AGENTS.md size budget: an index agents read in
// full, not a copy of the docs.
const initAgentsMaxBytes = 8 * 1024

// renderAllInitAgentsMD renders AGENTS.md for every target and language key,
// keyed by "target/language".
func renderAllInitAgentsMD(t *testing.T, appID string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, target := range []string{targetWendyOS, targetWendyLite, targetDarwin} {
		for language := range initLanguageNames {
			content, err := renderInitAgentsMD(appID, target, language)
			if err != nil {
				t.Fatalf("renderInitAgentsMD(%s, %s): %v", target, language, err)
			}
			out[target+"/"+language] = string(content)
		}
	}
	return out
}

func TestRenderInitAgentsMD_FitsSizeBudget(t *testing.T) {
	appID := "com.example.a-long-wendy-application-id"
	for variant, content := range renderAllInitAgentsMD(t, appID) {
		if len(content) > initAgentsMaxBytes {
			t.Errorf("%s: AGENTS.md is %d bytes, want at most %d", variant, len(content), initAgentsMaxBytes)
		}
		if !strings.Contains(content, "# "+appID+"\n") {
			t.Errorf("%s: AGENTS.md does not start with the app ID heading", variant)
		}
		if strings.Contains(content, "<no value>") || strings.Contains(content, "\n\n\n") {
			t.Errorf("%s: AGENTS.md has an unrendered value or a stray blank line:\n%s", variant, content)
		}
	}
}

func TestRenderInitAgentsMD_TailorsTargetAndLanguage(t *testing.T) {
	linux, err := renderInitAgentsMD("demo", targetWendyOS, langNode)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"written in Node (TypeScript)", "wendy run --detach --wait-ready", "wendy project add gpu", "guides/tutorials/typescript/hello-world/", "wendy device logs --app demo"} {
		if !strings.Contains(string(linux), want) {
			t.Errorf("WendyOS AGENTS.md is missing %q", want)
		}
	}

	lite, err := renderInitAgentsMD("demo", targetWendyLite, langSwift)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"--wait-ready", "wendy project add", "wendy device apps", "hello-world"} {
		if strings.Contains(string(lite), unwanted) {
			t.Errorf("Wendy Lite AGENTS.md mentions WendyOS-only %q", unwanted)
		}
	}
	if !strings.Contains(string(lite), "installation/wendy-lite-esp32/") {
		t.Error("Wendy Lite AGENTS.md does not link the ESP32 setup page")
	}
}

// docsURLPattern matches docs-site URLs up to Markdown syntax, a code span or
// a placeholder.
var docsURLPattern = regexp.MustCompile("https://docs\\.wendy\\.dev/latest/[^\\s<>()`]*")

// Every docs link in AGENTS.md must name a published, non-empty page, so the
// index fails here rather than going stale when a docs page moves.
func TestInitAgentsMD_DocsURLsExist(t *testing.T) {
	pages := publishedDocsPages(t)
	checked := map[string]bool{}
	for variant, content := range renderAllInitAgentsMD(t, "demo") {
		for _, url := range docsURLPattern.FindAllString(content, -1) {
			url = strings.TrimRight(url, ".,;:")
			if checked[url] {
				continue
			}
			checked[url] = true

			rel := strings.TrimPrefix(url, "https://docs.wendy.dev/latest/")
			var slug string
			switch {
			case rel == "llms.txt":
				continue // generated from every page by publish-markdown.mjs
			case strings.HasPrefix(rel, "markdown/") && strings.HasSuffix(rel, ".md"):
				slug = strings.TrimSuffix(strings.TrimPrefix(rel, "markdown/"), ".md")
				if slug == "index" {
					slug = ""
				}
			case rel == "" || strings.HasSuffix(rel, "/"):
				slug = strings.TrimSuffix(rel, "/")
			default:
				// Without the trailing slash, the static host redirects to
				// .../index.html, which the docs router can't match.
				t.Errorf("%s: %s does not end with /", variant, url)
				continue
			}

			src, ok := pages[slug]
			if !ok {
				t.Errorf("%s: %s is not a page in the docs tree", variant, url)
				continue
			}
			data, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(data)) == "" {
				t.Errorf("%s: %s links %s, which is empty", variant, url, src)
			}
		}
	}
	if len(checked) < 10 {
		t.Fatalf("found only %d docs URLs in AGENTS.md; did the URL pattern stop matching?", len(checked))
	}
}

// publishedDocsPages maps each docs-site URL path ("" for the home page,
// "device/entitlements", "advanced/apps/wendy.json") to its source file in
// the embedded docs tree. It mirrors docs/scripts/prepare-content.mjs and
// publish-markdown.mjs: .md pages move under advanced/, README.md becomes
// index.md, and an index page takes its directory's path.
func publishedDocsPages(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join("..", "assets", "docs")
	// prepare-content.mjs's skipDirs: the docs-site app and generated output.
	skipDirs := []string{".git", ".next", ".source", "app", "components", "content", "export", "lib", "node_modules", "out", "public", "screenshots", "scripts"}

	pages := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if p == root || name == "AGENTS.md" || name == "CLAUDE.md" {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(name, ".") || slices.Contains(skipDirs, name) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(path.Ext(name))
		if strings.HasPrefix(name, ".") || (ext != ".md" && ext != ".mdx") {
			return nil
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		target := filepath.ToSlash(rel)
		if strings.EqualFold(path.Base(target), "readme.md") {
			target = path.Join(path.Dir(target), "index.md")
		}
		if ext == ".md" {
			target = path.Join("advanced", target)
		}
		slug := strings.TrimSuffix(target, path.Ext(target))
		if slug == "index" {
			slug = ""
		}
		slug = strings.TrimSuffix(slug, "/index")
		pages[slug] = p
		return nil
	})
	if err != nil {
		t.Fatalf("walking the docs tree: %v", err)
	}
	if len(pages) < 100 {
		t.Fatalf("found only %d docs pages under %s", len(pages), root)
	}
	return pages
}

func TestWriteInitAgentsFiles_CreatesAgentsAndClaudeMD(t *testing.T) {
	dir := t.TempDir()
	files, err := writeInitAgentsFiles(dir, "demo-app", targetWendyOS, langPython)
	if err != nil {
		t.Fatalf("writeInitAgentsFiles: %v", err)
	}
	if want := []string{initAgentsFile, initClaudeFile}; !slices.Equal(files, want) {
		t.Fatalf("created %v, want %v", files, want)
	}

	agents, err := os.ReadFile(filepath.Join(dir, initAgentsFile))
	if err != nil {
		t.Fatal(err)
	}
	want, err := renderInitAgentsMD("demo-app", targetWendyOS, langPython)
	if err != nil {
		t.Fatal(err)
	}
	if string(agents) != string(want) {
		t.Errorf("AGENTS.md does not match the rendered template:\n%s", agents)
	}

	claude, err := os.ReadFile(filepath.Join(dir, initClaudeFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(claude) != "@AGENTS.md\n" {
		t.Errorf("CLAUDE.md = %q, want an @AGENTS.md import", claude)
	}
}

func TestWriteInitAgentsFiles_KeepsExistingAgentsMD(t *testing.T) {
	dir := t.TempDir()
	agentsPath := filepath.Join(dir, initAgentsFile)
	if err := os.WriteFile(agentsPath, []byte("my instructions\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := writeInitAgentsFiles(dir, "demo-app", targetWendyOS, langPython)
	if err != nil {
		t.Fatalf("writeInitAgentsFiles: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("created %v, want nothing", files)
	}
	if got, _ := os.ReadFile(agentsPath); string(got) != "my instructions\n" {
		t.Errorf("existing AGENTS.md was overwritten: %q", got)
	}
	// CLAUDE.md would import the user's file, not ours, so it is left alone.
	if _, err := os.Stat(filepath.Join(dir, initClaudeFile)); !os.IsNotExist(err) {
		t.Errorf("CLAUDE.md was created next to an existing AGENTS.md (stat err %v)", err)
	}
}

func TestWriteInitAgentsFiles_KeepsExistingClaudeMD(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, initClaudeFile)
	if err := os.WriteFile(claudePath, []byte("my Claude notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := writeInitAgentsFiles(dir, "demo-app", targetWendyOS, langPython)
	if err != nil {
		t.Fatalf("writeInitAgentsFiles: %v", err)
	}
	if want := []string{initAgentsFile}; !slices.Equal(files, want) {
		t.Fatalf("created %v, want %v", files, want)
	}
	if got, _ := os.ReadFile(claudePath); string(got) != "my Claude notes\n" {
		t.Errorf("existing CLAUDE.md was overwritten: %q", got)
	}
}

func TestInitCommand_NonInteractiveWritesAgentsMD(t *testing.T) {
	dir := chdirTemp(t)
	stubNonInteractive(t)

	cmd := newInitCmd()
	cmd.SetArgs([]string{
		"--app-id", "demo-app",
		"--target", "wendyos",
		"--language", "swift",
		"--no-extra-entitlements",
		"--assistant", "skip",
	})
	var execErr error
	stderr := captureStderr(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("Execute: %v\nstderr:\n%s", execErr, stderr)
	}

	agents, err := os.ReadFile(filepath.Join(dir, initAgentsFile))
	if err != nil {
		t.Fatalf("AGENTS.md not created: %v", err)
	}
	if !strings.Contains(string(agents), "# demo-app\n") || !strings.Contains(string(agents), "guides/tutorials/swift/hello-world/") {
		t.Errorf("AGENTS.md is not tailored to the Swift app:\n%s", agents)
	}
	if _, err := os.Stat(filepath.Join(dir, initClaudeFile)); err != nil {
		t.Errorf("CLAUDE.md not created: %v", err)
	}
	if !strings.Contains(stderr, "Created AGENTS.md and CLAUDE.md") {
		t.Errorf("init output does not report the agent files:\n%s", stderr)
	}
}
