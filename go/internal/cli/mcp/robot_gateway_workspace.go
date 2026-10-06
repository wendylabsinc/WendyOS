package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

var gatewayWorkspaceFileTools = []string{"list_workspace_files", "read_workspace_file", "write_workspace_file"}

const gatewayWorkspaceFileLimit = 256 << 10

func (g *RobotGateway) localWorkspaceAllowed(ctx context.Context, scope string) bool {
	_, principal := g.grant(ctx)
	return ctx.Value(gatewayLocalContextKey{}) == true && principal.Subject == g.cfg.LocalSubject && g.hasScope(ctx, scope)
}

func (g *RobotGateway) registerWorkspaceFileTools() {
	for _, name := range gatewayWorkspaceFileTools {
		behavior := readOnly()
		description := "List one directory of an approved local workspace. Returns sorted relative names and types, up to 200 entries. Follow next_offset for more entries, or use subdirectory paths to explore the project. Does not execute code."
		path := mcpgo.WithString("path", mcpgo.DefaultString("."), mcpgo.MaxLength(1024))
		if name != "list_workspace_files" {
			path = mcpgo.WithString("path", mcpgo.Required(), mcpgo.MinLength(1), mcpgo.MaxLength(1024))
			description = "Read a UTF-8 project file within an approved local workspace, up to 256 KiB. Returns content and its SHA-256 for a subsequent edit. Does not execute code."
		}
		opts := []mcpgo.ToolOption{mcpgo.WithString("workspace_id", mcpgo.Required(), mcpgo.MaxLength(64)), path}
		if name == "list_workspace_files" {
			opts = append(opts, mcpgo.WithInteger("offset", mcpgo.Min(0), mcpgo.Max(10000), mcpgo.DefaultNumber(0)))
		}
		if name == "write_workspace_file" {
			behavior = mutating()
			description = "Create or replace a UTF-8 project file within an approved local workspace, up to 256 KiB. Requires projects:write. Supply expected_sha256 from read_workspace_file, or 'missing' to create a new file. A stale revision fails without writing. Creates parent directories. Does not build or deploy; validate then use start_device_deployment with an explicit robot_id."
			opts = append(opts, mcpgo.WithString("content", mcpgo.Required()), mcpgo.WithString("expected_sha256", mcpgo.Required(), mcpgo.MaxLength(64)))
		}
		g.protocol.AddTool(gatewayTool(name, description, behavior, opts...), g.handleWorkspaceFile)
	}
}

func workspaceRelativePath(path string, directory bool) (string, error) {
	if len(path) > 1024 || !filepath.IsLocal(path) || strings.ContainsAny(path, "\\\x00") {
		return "", fmt.Errorf("path must be relative to the approved workspace")
	}
	// Never expose repository control files or normalize a traversal away.
	for _, component := range strings.Split(path, "/") {
		if component == ".." || strings.EqualFold(component, ".git") {
			return "", fmt.Errorf("parent traversal and .git access are not permitted")
		}
	}
	path = filepath.Clean(path)
	if path == "." && !directory {
		return "", fmt.Errorf("select a project file")
	}
	return path, nil
}

func workspacePathHasLinks(root *os.Root, path string) bool {
	current := "."
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if os.IsNotExist(err) {
			return false // New files and parent directories are allowed.
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

func readWorkspaceText(root *os.Root, path string) ([]byte, os.FileMode, error) {
	// Reject symlinks even within the root, so a write and a read name the same
	// file. Root also prevents symlink races from escaping through a parent.
	info, err := root.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() > gatewayWorkspaceFileLimit {
		return nil, 0, fmt.Errorf("select a regular UTF-8 file of at most 256 KiB")
	}
	f, err := root.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, gatewayWorkspaceFileLimit+1))
	if err != nil {
		return nil, 0, err
	}
	if len(data) > gatewayWorkspaceFileLimit || !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') {
		return nil, 0, fmt.Errorf("select a regular UTF-8 file of at most 256 KiB")
	}
	return data, info.Mode().Perm(), nil
}

func workspaceDigest(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (g *RobotGateway) handleWorkspaceFile(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if !g.localWorkspaceAllowed(ctx, g.toolScope(req.Params.Name)) {
		return mcpgo.NewToolResultError("Project files require an authorized local stdio workspace session."), nil
	}
	w, err := g.workspace(ctx, req.GetString("workspace_id", ""), "")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	path, err := workspaceRelativePath(req.GetString("path", "."), req.Params.Name == "list_workspace_files")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	root, err := os.OpenRoot(w.Path)
	if err != nil {
		return mcpgo.NewToolResultError("Approved workspace is unavailable on this gateway host."), nil
	}
	defer root.Close()
	// Serialize revision checks and writes from concurrent conversations.
	g.workspaceMu.Lock()
	defer g.workspaceMu.Unlock()
	if workspacePathHasLinks(root, path) {
		return mcpgo.NewToolResultError("Select a workspace path without symbolic links."), nil
	}
	if req.Params.Name == "list_workspace_files" {
		offset, err := ros2Int(req, "offset", 0, 0, 10000)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		f, err := root.Open(path)
		if err != nil {
			return mcpgo.NewToolResultError("Workspace directory is unavailable."), nil
		}
		defer f.Close()
		entries, err := f.ReadDir(10001)
		if err != nil && err != io.EOF {
			return mcpgo.NewToolResultError("Could not list this workspace directory."), nil
		}
		if len(entries) > 10000 {
			return mcpgo.NewToolResultError("Directory exceeds 10000 entries. Select a project subdirectory instead."), nil
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		rows := []map[string]any{}
		for _, entry := range entries {
			if entry.Name() == ".git" {
				continue
			}
			kind := "file"
			if entry.IsDir() {
				kind = "directory"
			} else if entry.Type()&os.ModeSymlink != 0 {
				kind = "symlink"
			}
			rows = append(rows, map[string]any{"path": filepath.ToSlash(filepath.Join(path, entry.Name())), "type": kind})
		}
		start := min(offset, len(rows))
		end := min(start+200, len(rows))
		var next any
		if end < len(rows) {
			next = end
		}
		return okResult(map[string]any{"workspace_id": w.ID, "path": path, "entries": rows[start:end], "next_offset": next, "truncated": end < len(rows)}), nil
	}
	data, mode, err := readWorkspaceText(root, path)
	if req.Params.Name == "read_workspace_file" {
		if err != nil {
			return mcpgo.NewToolResultError("File is unavailable, outside the workspace, or not a regular UTF-8 file of at most 256 KiB."), nil
		}
		return okResult(map[string]any{"workspace_id": w.ID, "path": path, "content": string(data), "sha256": workspaceDigest(data)}), nil
	}
	content, ok := req.GetArguments()["content"].(string)
	if !ok || len(content) > gatewayWorkspaceFileLimit || !utf8.ValidString(content) || strings.ContainsRune(content, '\x00') {
		return mcpgo.NewToolResultError("content must be UTF-8 text of at most 256 KiB"), nil
	}
	expected := req.GetString("expected_sha256", "")
	if os.IsNotExist(err) {
		if expected != "missing" {
			return mcpgo.NewToolResultError("File revision changed. Read the file before editing; use 'missing' only to create it."), nil
		}
		mode = 0644
	} else if err != nil {
		return mcpgo.NewToolResultError("File cannot be edited within this workspace."), nil
	} else if expected != workspaceDigest(data) {
		return mcpgo.NewToolResultError("File revision changed. Read the current file before editing."), nil
	}
	if err := root.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return mcpgo.NewToolResultError("Could not create project directories."), nil
	}
	temporary := filepath.Join(filepath.Dir(path), ".wendy-edit-"+rand.Text())
	f, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return mcpgo.NewToolResultError("Could not write project file."), nil
	}
	defer root.Remove(temporary)
	writeErr := f.Chmod(mode)
	if writeErr == nil {
		_, writeErr = f.WriteString(content)
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return mcpgo.NewToolResultError("Could not write project file."), nil
	}
	if err := root.Rename(temporary, path); err != nil {
		return mcpgo.NewToolResultError("Could not replace project file."), nil
	}
	return okResult(map[string]any{"workspace_id": w.ID, "path": path, "sha256": workspaceDigest([]byte(content)), "status": "written"}), nil
}
