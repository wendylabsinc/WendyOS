package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/wendylabsinc/wendy/internal/cli/tui"
)

const (
	templateRepoOwner  = "wendylabsinc"
	templateRepoName   = "templates"
	templateRepoBranch = "main"
)

var (
	templateArchiveAttemptTimeout = 2 * time.Minute
	templateArchiveMaxAttempts    = 3
	templateArchiveRetryDelay     = 750 * time.Millisecond
)

// resolveTemplateBranch returns branch if non-empty, otherwise the default branch.
func resolveTemplateBranch(branch string) string {
	if branch == "" {
		return templateRepoBranch
	}
	return branch
}

// repoMeta is the parsed meta.json from the templates repo root.
type repoMeta struct {
	Templates []repoMetaTemplate `json:"templates"`
	Languages []repoMetaLanguage `json:"languages"`
}

type repoMetaTemplate struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Targets     []string `json:"targets"` // optional; empty means all targets
}

type repoMetaLanguage struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// templateManifest is the parsed template.json inside a specific template dir.
type templateManifest struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Variables   []templateVariable `json:"variables"`
}

// templateVariable declares a single template variable.
type templateVariable struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Type        string                 `json:"type"` // "string", "integer", "boolean"
	Default     interface{}            `json:"default"`
	Required    bool                   `json:"required"`
	Prompt      string                 `json:"prompt"`
	Validate    map[string]interface{} `json:"validate"`
}

// fetchRepoMeta downloads and parses meta.json from the templates repo.
// If branch is empty, it defaults to templateRepoBranch ("main").
// If ctx is cancelled, the in-flight request is aborted.
func fetchRepoMeta(ctx context.Context, branch string) (*repoMeta, error) {
	branch = resolveTemplateBranch(branch)
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/meta.json",
		templateRepoOwner, templateRepoName, branch)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching template registry (branch %q): %w", branch, err)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching template registry (branch %q): %w", branch, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("template registry not found for branch %q — check that the branch exists in %s/%s",
			branch, templateRepoOwner, templateRepoName)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching template registry (branch %q): HTTP %d", branch, resp.StatusCode)
	}

	var meta repoMeta
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("parsing template registry: %w", err)
	}
	return &meta, nil
}

// isTemplateLanguage checks if a language key exists in the meta.
func isTemplateLanguage(language string, meta *repoMeta) bool {
	for _, l := range meta.Languages {
		if l.Key == language {
			return true
		}
	}
	return false
}

// progressCallback reports download progress. total is the expected content
// length in bytes (0 if unknown); written is the cumulative number of bytes
// read from the response body so far.
type progressCallback func(written, total int64)

// progressReader wraps an io.Reader and invokes onProgress after each Read.
type progressReader struct {
	r          io.Reader
	total      int64
	written    int64
	onProgress progressCallback
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 {
		pr.written += int64(n)
		if pr.onProgress != nil {
			pr.onProgress(pr.written, pr.total)
		}
	}
	return n, err
}

// downloadTemplateArchive fetches the templates repo tarball and extracts
// the files for {language}/{templateName}/ into a map of relative path -> content.
// It also returns the parsed template.json manifest.
// If branch is empty, it defaults to templateRepoBranch ("main").
// If onProgress is non-nil, it is invoked as the response body is read.
// If ctx is cancelled, the in-flight request is aborted.
func downloadTemplateArchive(ctx context.Context, language, templateName, branch string, onProgress progressCallback) (map[string][]byte, *templateManifest, error) {
	branch = resolveTemplateBranch(branch)
	// Use codeload directly to avoid an extra redirect through github.com for the
	// repository archive download.
	url := fmt.Sprintf("https://codeload.github.com/%s/%s/tar.gz/refs/heads/%s",
		templateRepoOwner, templateRepoName, branch)
	return downloadTemplateArchiveFromURL(ctx, url, branch, language, templateName, onProgress)
}

// downloadTemplateArchiveFromURL is the testable core of downloadTemplateArchive:
// it performs the HTTP GET against the caller-supplied URL and delegates
// tarball parsing to extractTemplateArchive.
func downloadTemplateArchiveFromURL(ctx context.Context, url, branch, language, templateName string, onProgress progressCallback) (map[string][]byte, *templateManifest, error) {
	var lastErr error
	for attempt := 1; attempt <= templateArchiveMaxAttempts; attempt++ {
		files, manifest, err := downloadTemplateArchiveAttempt(ctx, url, branch, language, templateName, onProgress)
		if err == nil {
			return files, manifest, nil
		}
		lastErr = err

		if ctx.Err() != nil || attempt == templateArchiveMaxAttempts || !shouldRetryTemplateArchiveError(err) {
			return nil, nil, err
		}

		if err := waitForTemplateArchiveRetry(ctx, templateArchiveRetryDelay); err != nil {
			return nil, nil, err
		}
	}

	return nil, nil, lastErr
}

func downloadTemplateArchiveAttempt(ctx context.Context, url, branch, language, templateName string, onProgress progressCallback) (map[string][]byte, *templateManifest, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, templateArchiveAttemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("downloading template (branch %q): %w", branch, err)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("downloading template (branch %q): %w", branch, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, fmt.Errorf("template archive not found for branch %q — check that the branch exists in %s/%s",
			branch, templateRepoOwner, templateRepoName)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("downloading template (branch %q): HTTP %d", branch, resp.StatusCode)
	}

	var reader io.Reader = resp.Body
	if onProgress != nil {
		// Normalize ContentLength to the progressCallback contract:
		// http.Response.ContentLength is -1 when unknown, but callers expect 0.
		total := resp.ContentLength
		if total < 0 {
			total = 0
		}
		reader = &progressReader{
			r:          resp.Body,
			total:      total,
			onProgress: onProgress,
		}
	}

	return extractTemplateArchive(reader, language, templateName)
}

func shouldRetryTemplateArchiveError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	msg := err.Error()
	return strings.Contains(msg, "Client.Timeout") || strings.Contains(msg, "while reading body")
}

func waitForTemplateArchiveRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// extractTemplateArchive reads a gzipped tarball from r and extracts files
// matching {language}/{templateName}/ into a map of relative path -> content.
// It also returns the parsed template.json manifest.
func extractTemplateArchive(r io.Reader, language, templateName string) (map[string][]byte, *templateManifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, fmt.Errorf("decompressing template archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	// The tarball has a top-level dir like "templates-main/".
	// We want files under "templates-main/{language}/{templateName}/".
	prefix := language + "/" + templateName + "/"

	files := make(map[string][]byte)
	var manifest *templateManifest

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading template archive: %w", err)
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		// Strip the top-level directory.
		name := header.Name
		slashIdx := strings.Index(name, "/")
		if slashIdx < 0 {
			continue
		}
		name = name[slashIdx+1:]

		if !strings.HasPrefix(name, prefix) {
			continue
		}

		relPath := strings.TrimPrefix(name, prefix)
		if relPath == "" {
			continue
		}

		// Sanitize: reject path traversal.
		cleaned := filepath.Clean(relPath)
		if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
			continue
		}
		relPath = cleaned

		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, fmt.Errorf("reading file %s: %w", relPath, err)
		}

		if relPath == "template.json" {
			var m templateManifest
			if err := json.Unmarshal(content, &m); err != nil {
				return nil, nil, fmt.Errorf("parsing template.json: %w", err)
			}
			manifest = &m
			continue // don't include template.json in output files
		}

		files[relPath] = content
	}

	if manifest == nil {
		return nil, nil, fmt.Errorf("template %q not found for language %q (no template.json)", templateName, language)
	}

	return files, manifest, nil
}

// collectTemplateValues gathers values for all template variables.
// It uses varOverrides (from --var flags) for non-interactive values,
// falling back to bubbletea prompts for anything missing.
// If all variables can be resolved without prompting (via overrides or defaults),
// no interactive prompts are shown. Otherwise all non-overridden variables are
// prompted interactively with defaults pre-filled.
func collectTemplateValues(manifest *templateManifest, appID string, varOverrides map[string]string) (map[string]interface{}, error) {
	vals := map[string]interface{}{
		"APP_ID": appID,
	}

	// Determine if any variables need interactive input.
	needsPrompt := false
	for _, v := range manifest.Variables {
		if v.Name == "APP_ID" {
			continue
		}
		if _, ok := varOverrides[v.Name]; ok {
			continue
		}
		if v.Default == nil {
			needsPrompt = true
			break
		}
	}

	for _, v := range manifest.Variables {
		if v.Name == "APP_ID" {
			continue
		}

		// Check --var overrides first.
		if raw, ok := varOverrides[v.Name]; ok {
			parsed, err := parseVariableValue(v, raw)
			if err != nil {
				return nil, fmt.Errorf("invalid value for %s: %w", v.Name, err)
			}
			if err := validateVariable(v, parsed); err != nil {
				return nil, err
			}
			vals[v.Name] = parsed
			continue
		}

		// If no prompting needed, use defaults silently.
		if !needsPrompt && v.Default != nil {
			vals[v.Name] = v.Default
			continue
		}

		// Interactive prompt with default pre-filled.
		val, err := promptForVariable(v)
		if err != nil {
			return nil, err
		}
		vals[v.Name] = val
	}

	return vals, nil
}

// promptForVariable shows a bubbletea prompt for a single template variable.
func promptForVariable(v templateVariable) (interface{}, error) {
	prompt := v.Prompt
	if prompt == "" {
		prompt = v.Name
	}

	switch v.Type {
	case "boolean":
		defVal := false
		if b, ok := v.Default.(bool); ok {
			defVal = b
		}
		_ = defVal // tui.Confirm doesn't support defaults, so we just ask
		result, err := tui.Confirm(prompt + "?")
		if err != nil {
			return nil, err
		}
		return result, nil

	case "integer":
		defStr := ""
		if v.Default != nil {
			defStr = fmt.Sprintf("%v", v.Default)
			// JSON numbers unmarshal as float64.
			if f, ok := v.Default.(float64); ok {
				defStr = strconv.Itoa(int(f))
			}
		}

		validate := func(input string) error {
			n, err := strconv.Atoi(strings.TrimSpace(input))
			if err != nil {
				return fmt.Errorf("must be an integer")
			}
			return validateVariable(v, n)
		}

		var result string
		var err error
		if defStr != "" {
			result, err = tui.PromptTextWithDefault(prompt, v.Description, defStr, validate)
		} else {
			result, err = tui.PromptText(prompt, v.Description, validate)
		}
		if err != nil {
			return nil, err
		}
		n, _ := strconv.Atoi(strings.TrimSpace(result))
		return n, nil

	default: // "string"
		defStr := ""
		if s, ok := v.Default.(string); ok {
			defStr = s
		}

		validate := func(input string) error {
			if v.Required && strings.TrimSpace(input) == "" {
				return fmt.Errorf("%s cannot be empty", prompt)
			}
			return validateVariable(v, strings.TrimSpace(input))
		}

		var result string
		var err error
		if defStr != "" {
			result, err = tui.PromptTextWithDefault(prompt, v.Description, defStr, validate)
		} else {
			result, err = tui.PromptText(prompt, v.Description, validate)
		}
		if err != nil {
			return nil, err
		}
		return strings.TrimSpace(result), nil
	}
}

// parseVariableValue converts a string flag value to the appropriate Go type.
func parseVariableValue(v templateVariable, raw string) (interface{}, error) {
	switch v.Type {
	case "integer":
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("expected integer, got %q", raw)
		}
		return n, nil
	case "boolean":
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("expected boolean, got %q", raw)
		}
		return b, nil
	default:
		return raw, nil
	}
}

// validateVariable checks a value against the variable's validation rules.
func validateVariable(v templateVariable, val interface{}) error {
	if v.Validate == nil {
		return nil
	}

	switch v.Type {
	case "integer":
		n, ok := val.(int)
		if !ok {
			return nil
		}
		if minRaw, ok := v.Validate["min"]; ok {
			if minF, ok := minRaw.(float64); ok && n < int(minF) {
				return fmt.Errorf("%s must be at least %d", v.Name, int(minF))
			}
		}
		if maxRaw, ok := v.Validate["max"]; ok {
			if maxF, ok := maxRaw.(float64); ok && n > int(maxF) {
				return fmt.Errorf("%s must be at most %d", v.Name, int(maxF))
			}
		}

	case "string":
		s, ok := val.(string)
		if !ok {
			return nil
		}
		if patternRaw, ok := v.Validate["pattern"]; ok {
			if pattern, ok := patternRaw.(string); ok {
				re, err := regexp.Compile(pattern)
				if err != nil {
					return fmt.Errorf("invalid validation pattern %q: %w", pattern, err)
				}
				if !re.MatchString(s) {
					return fmt.Errorf("%s does not match pattern %s", v.Name, pattern)
				}
			}
		}
	}

	return nil
}

// renderAndWriteTemplate takes the raw file map, evaluates each text file as a
// Go text/template (so {{.VAR}}, {{if}}, {{range}}, etc. all work), and writes
// to destDir. It renames directories named after the template to the app ID.
func renderAndWriteTemplate(files map[string][]byte, destDir, appID, templateName string, vals map[string]interface{}) error {
	for relPath, content := range files {
		// Rename template-named directories to app ID.
		relPath = renameTemplatePath(relPath, templateName, appID)

		destPath := filepath.Join(destDir, relPath)

		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return fmt.Errorf("creating directory for %s: %w", relPath, err)
		}

		// Only render text files. Binary files (images, fonts, wasm) are written as-is.
		output := content
		if isTextFile(relPath) {
			rendered, err := renderTemplateContent(relPath, content, vals)
			if err != nil {
				return err
			}
			output = rendered
		}

		if err := os.WriteFile(destPath, output, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", destPath, err)
		}
	}

	return nil
}

// renderTemplateContent evaluates content as a Go text/template against vals.
// Parse errors are surfaced (scoped to path) so template-authoring mistakes
// like a broken {{if}} don't silently produce files with unrendered actions.
// missingkey=error causes references to undeclared variables to fail rather
// than render as "<no value>".
func renderTemplateContent(path string, content []byte, vals map[string]interface{}) ([]byte, error) {
	tmpl, err := template.New(path).Option("missingkey=error").Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vals); err != nil {
		return nil, fmt.Errorf("rendering %s: %w", path, err)
	}
	return buf.Bytes(), nil
}

// renameTemplatePath replaces occurrences of the template name in path
// components with the app ID (e.g. Sources/simple-api/ -> Sources/my-app/).
func renameTemplatePath(relPath, templateName, appID string) string {
	parts := strings.Split(relPath, "/")
	for i, part := range parts {
		if part == templateName {
			parts[i] = appID
		}
	}
	return strings.Join(parts, "/")
}

// isTextFile returns true if a file path looks like a text file that should
// have template tokens replaced. Binary files are left as-is.
func isTextFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".json", ".toml", ".yaml", ".yml", ".md", ".txt", ".html", ".css",
		".js", ".ts", ".tsx", ".jsx", ".py", ".rs", ".swift", ".go",
		".cpp", ".c", ".h", ".hpp", ".cmake", ".sh", ".bash", ".zsh",
		".dockerfile", ".gitignore", ".env", ".cfg", ".ini", ".xml",
		".svg", ".lock":
		return true
	}
	// Files without extension (Dockerfile, Makefile, etc.)
	base := filepath.Base(path)
	switch base {
	case "Dockerfile", "Makefile", "CMakeLists.txt", "Package.swift",
		"Cargo.toml", "Cargo.lock", ".swift-version", ".gitignore":
		return true
	}
	return false
}

// parseVarFlags parses --var KEY=VALUE flags into a map.
func parseVarFlags(vars []string) (map[string]string, error) {
	result := make(map[string]string, len(vars))
	for _, v := range vars {
		eq := strings.IndexByte(v, '=')
		if eq < 1 {
			return nil, fmt.Errorf("invalid --var format %q (expected KEY=VALUE)", v)
		}
		key := strings.TrimSpace(v[:eq])
		if key == "" {
			return nil, fmt.Errorf("invalid --var format %q (empty key)", v)
		}
		val := v[eq+1:]
		result[key] = val
	}
	return result, nil
}
