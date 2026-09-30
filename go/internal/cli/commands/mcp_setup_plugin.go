package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Notes on an mcpSetupResult for a client whose Wendy plugin already runs the
// MCP server (see setupMCPForAllTools).
const (
	mcpNoteSkippedForPlugin = "skipped-for-plugin"
	mcpNoteRemovedForPlugin = "removed-for-plugin"
)

// mcpSetupResultLine renders one result of `wendy mcp setup`.
func mcpSetupResultLine(r mcpSetupResult) string {
	switch {
	case r.err != nil:
		return fmt.Sprintf("✗ %s: %v", r.tool, r.err)
	case r.note == mcpNoteRemovedForPlugin:
		return fmt.Sprintf("✓ %s: removed the user-level wendy server (the plugin provides it)", r.tool)
	case r.note == mcpNoteSkippedForPlugin:
		return fmt.Sprintf("↷ %s: skipped — the Wendy plugin provides the MCP server", r.tool)
	default:
		return fmt.Sprintf("✓ %s: configured at %s", r.tool, r.path)
	}
}

// pluginProvidedResult is setup's result for a client whose Wendy plugin
// already runs the MCP server. Setup writes no user-level entry there — both
// would list every Wendy tool twice — and remove takes out the one an earlier
// setup wrote, reporting whether there was one.
func pluginProvidedResult(tool, path string, remove func() (bool, error)) mcpSetupResult {
	removed, err := remove()
	switch {
	case err != nil:
		return mcpSetupResult{tool: tool, path: path, err: fmt.Errorf("the Wendy plugin provides the MCP server, but removing the user-level wendy server failed: %w", err)}
	case removed:
		return mcpSetupResult{tool: tool, path: path, note: mcpNoteRemovedForPlugin}
	default:
		return mcpSetupResult{tool: tool, path: path, note: mcpNoteSkippedForPlugin}
	}
}

// removeMCPFromJSONConfig deletes cfg[topKey][name] from the JSON file at path
// when it is an entry setup wrote (its args start with "mcp serve") and
// reports whether it did. Numbers are kept exactly as written (large
// timestamps and counters in ~/.claude.json exceed float64 precision), HTML
// characters are not escaped, the file keeps its mode, a symlink keeps
// pointing at the edited target, and the write is atomic. A missing or empty
// file is not an error.
func removeMCPFromJSONConfig(path, topKey, name string) (bool, error) {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	data, err := os.ReadFile(target)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return false, nil
	}
	var cfg map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&cfg); err != nil {
		return false, fmt.Errorf("parsing %s: %w", path, err)
	}
	top, _ := cfg[topKey].(map[string]any)
	entry, ok := top[name].(map[string]any)
	if !ok || !mcpArgsStartWith(entry["args"], mcpServeArgs) {
		return false, nil
	}
	delete(top, name)

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return false, err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(target); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := writeFileAtomic(target, out.Bytes(), mode); err != nil {
		return false, err
	}
	return true, nil
}
