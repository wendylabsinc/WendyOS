package chat

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/assets"
)

// Tools combines project development tools with the Wendy MCP server. Only the
// trusted, bundled MCP server is launched; workspace files cannot add servers.
type Tools struct {
	workspace string
	root      *os.Root
	mcp       mcpToolClient
	stderr    *limitedBuffer
	mu        sync.RWMutex
	known     map[string]Tool
	closeOnce sync.Once
	closeErr  error
}

var localTools = []Tool{
	{
		Name: "workspace_list", Description: "List entries in a local project directory. Paths are relative to the workspace; returns at most 1000 entries.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Workspace-relative directory, default ."}},"additionalProperties":false}`),
	},
	{
		Name: "workspace_read", Description: "Read a UTF-8 local project file, with 1-based line numbers. Use offset and limit for long files; output is limited to 32 KiB.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1},"offset":{"type":"integer","minimum":1,"maximum":1000000},"limit":{"type":"integer","minimum":1,"maximum":500}},"required":["path"],"additionalProperties":false}`),
	},
	{
		Name: "workspace_write", Description: "Create or replace a local project file with the complete supplied UTF-8 content. Creates parent directories. Read existing files and project instructions before editing. Requires user approval.", RequiresApproval: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1},"content":{"type":"string","maxLength":1048576}},"required":["path","content"],"additionalProperties":false}`),
	},
	{
		Name: "workspace_exec", Description: "Run a shell command on this computer for builds, tests, git, or other development tasks. Starts in the workspace (or cwd within it). This is not a sandbox: the command can access the computer and network. Requires explicit user approval. Output is capped at 32 KiB; default timeout is 120 seconds, maximum 600. Do not start background daemons.", RequiresApproval: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","minLength":1},"cwd":{"type":"string","description":"Workspace-relative working directory, default ."},"timeout_seconds":{"type":"integer","minimum":1,"maximum":600}},"required":["command"],"additionalProperties":false}`),
	},
	{
		Name: "wendy_docs", Description: "Read the Wendy documentation bundled with this CLI. With no path, list available documentation paths; provide a listed path to read it. Output is capped at 32 KiB; offset and limit select lines.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":1,"maximum":1000000},"limit":{"type":"integer","minimum":1,"maximum":500}},"additionalProperties":false}`),
	},
}

func NewTools(ctx context.Context, executable, workspace, device string) (*Tools, error) {
	t, err := newWorkspaceTools(workspace)
	if err != nil {
		return nil, err
	}
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			_ = t.Close()
			return nil, fmt.Errorf("locating Wendy executable: %w", err)
		}
	}
	// Resolve before setting the child directory, so relative executable paths
	// always refer to the caller's directory rather than the project directory.
	executable, err = exec.LookPath(executable)
	if err == nil {
		executable, err = filepath.Abs(executable)
	}
	if err != nil {
		_ = t.Close()
		return nil, fmt.Errorf("locating Wendy executable: %w", err)
	}
	if err := t.startMCP(ctx, executable, device); err != nil {
		_ = t.Close()
		return nil, err
	}
	if _, err := t.ListTools(ctx); err != nil {
		_ = t.Close()
		return nil, err
	}
	return t, nil
}

func newWorkspaceTools(workspace string) (*Tools, error) {
	if workspace == "" {
		workspace = "."
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace: %w", err)
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace: %w", err)
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("opening workspace: %w", err)
	}
	t := &Tools{workspace: absolute, root: root, known: make(map[string]Tool), stderr: &limitedBuffer{limit: 4096}}
	for _, tool := range localTools {
		t.known[tool.Name] = tool
	}
	return t, nil
}

func (t *Tools) Close() error {
	t.closeOnce.Do(func() {
		if t.mcp != nil {
			t.closeErr = t.mcp.Close()
		}
		if t.root != nil {
			t.closeErr = errors.Join(t.closeErr, t.root.Close())
		}
	})
	return t.closeErr
}

func (t *Tools) Execute(ctx context.Context, call ToolCall) (string, error) {
	result, err := t.ExecuteResult(ctx, call)
	return result.Text, err
}

func (t *Tools) ExecuteResult(ctx context.Context, call ToolCall) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	t.mu.RLock()
	tool, ok := t.known[call.Name]
	t.mu.RUnlock()
	if !ok {
		return ToolResult{}, fmt.Errorf("unknown tool %q", call.Name)
	}
	if err := validateArguments(tool, call.Arguments); err != nil {
		return ToolResult{}, err
	}
	var output string
	var err error
	switch call.Name {
	case "workspace_list":
		var args struct{ Path string }
		_ = json.Unmarshal(call.Arguments, &args)
		output, err = t.listWorkspace(ctx, args.Path)
	case "workspace_read":
		var args struct {
			Path          string
			Offset, Limit int
		}
		_ = json.Unmarshal(call.Arguments, &args)
		output, err = t.readWorkspace(ctx, args.Path, args.Offset, args.Limit)
	case "workspace_write":
		var args struct{ Path, Content string }
		_ = json.Unmarshal(call.Arguments, &args)
		output, err = t.writeWorkspace(ctx, args.Path, args.Content)
	case "workspace_exec":
		var args struct {
			Command        string `json:"command"`
			CWD            string `json:"cwd"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		_ = json.Unmarshal(call.Arguments, &args)
		output, err = t.execWorkspace(ctx, args.Command, args.CWD, args.TimeoutSeconds)
	case "wendy_docs":
		var args struct {
			Path          string
			Offset, Limit int
		}
		_ = json.Unmarshal(call.Arguments, &args)
		output, err = readWendyDocs(ctx, args.Path, args.Offset, args.Limit)
	default:
		return t.callMCPResult(ctx, call)
	}
	return ToolResult{Text: truncateOutput(output)}, err
}

// os.Root enforces traversal and symlink boundaries on the actual file access,
// preventing a symlink replacement between validation and opening a file.
func workspacePath(name string) (string, error) {
	if name == "" {
		return ".", nil
	}
	if !filepath.IsLocal(name) {
		return "", errors.New("path must be relative to and inside the workspace")
	}
	return filepath.Clean(name), nil
}

func (t *Tools) listWorkspace(ctx context.Context, name string) (string, error) {
	name, err := workspacePath(name)
	if err != nil {
		return "", err
	}
	dir, err := t.root.Open(name)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(1001)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var out strings.Builder
	for i, entry := range entries {
		if err := ctx.Err(); err != nil {
			return out.String(), err
		}
		if i == 1000 {
			out.WriteString("[Directory listing truncated after 1000 entries.]\n")
			break
		}
		suffix := ""
		if entry.IsDir() {
			suffix = "/"
		} else if entry.Type()&os.ModeSymlink != 0 {
			suffix = " [symlink]"
		}
		out.WriteString(entry.Name() + suffix + "\n")
	}
	if len(entries) == 0 {
		out.WriteString("[Empty directory.]\n")
	}
	return out.String(), nil
}

func (t *Tools) readWorkspace(ctx context.Context, name string, offset, limit int) (string, error) {
	name, err := workspacePath(name)
	if err != nil {
		return "", err
	}
	info, err := t.root.Stat(name)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("only regular files can be read")
	}
	file, err := t.root.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return readLines(ctx, file, offset, limit)
}

func readLines(ctx context.Context, reader io.Reader, offset, limit int) (string, error) {
	if offset == 0 {
		offset = 1
	}
	if limit == 0 {
		limit = 200
	}
	// Bound both output and work needed to seek to an offset in a huge file.
	limited := &io.LimitedReader{R: reader, N: 16 * 1024 * 1024}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var out strings.Builder
	line := 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return out.String(), err
		}
		line++
		if line < offset {
			continue
		}
		if strings.IndexByte(scanner.Text(), 0) >= 0 {
			return "", errors.New("file appears to contain binary data")
		}
		fmt.Fprintf(&out, "%d: %s\n", line, scanner.Text())
		if line >= offset+limit-1 || out.Len() >= maxToolOutputBytes {
			out.WriteString("[Read limit reached; use offset to continue.]\n")
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return out.String(), fmt.Errorf("reading file: %w", err)
	}
	if limited.N == 0 {
		out.WriteString("[File scanning stopped at 16 MiB.]\n")
	}
	if out.Len() == 0 {
		out.WriteString("[No lines in the requested range.]\n")
	}
	return out.String(), nil
}

func (t *Tools) writeWorkspace(ctx context.Context, name, content string) (string, error) {
	if len(content) > 1024*1024 {
		return "", errors.New("file content exceeds the 1 MiB limit")
	}
	name, err := workspacePath(name)
	if err != nil {
		return "", err
	}
	mode := os.FileMode(0644)
	if info, err := t.root.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return "", errors.New("write target must be a regular file, not a directory or symbolic link")
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(name)
	if err := t.root.MkdirAll(parent, 0755); err != nil {
		return "", err
	}
	temporary := filepath.Join(parent, ".wendy-chat-"+rand.Text())
	file, err := t.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return "", err
	}
	defer t.root.Remove(temporary)
	_, writeErr := io.WriteString(file, content)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr, ctx.Err()); err != nil {
		return "", err
	}
	if err := t.root.Rename(temporary, name); err != nil {
		return "", err
	}
	return fmt.Sprintf("Wrote %d bytes to %s.", len(content), name), nil
}

func (t *Tools) execWorkspace(ctx context.Context, command, cwd string, timeoutSeconds int) (string, error) {
	cwd, err := workspacePath(cwd)
	if err != nil {
		return "", err
	}
	// Shell commands are explicitly approved and are not sandboxed, but their
	// initial directory must still resolve inside the selected workspace.
	directory, err := t.root.OpenRoot(cwd)
	if err != nil {
		return "", err
	}
	_ = directory.Close()
	resolved, err := filepath.EvalSymlinks(filepath.Join(t.workspace, cwd))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(t.workspace, resolved)
	if err != nil || !filepath.IsLocal(relative) {
		return "", errors.New("working directory must be inside the workspace")
	}
	if timeoutSeconds == 0 {
		timeoutSeconds = 120
	}
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	cmd := shellCommand(execCtx, command)
	cmd.Dir = resolved
	output := &limitedBuffer{limit: maxToolOutputBytes}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	if execCtx.Err() != nil {
		err = execCtx.Err()
	}
	if err != nil {
		return output.String(), fmt.Errorf("command failed: %w", err)
	}
	if output.String() == "" {
		return "Command completed successfully with no output.", nil
	}
	return output.String(), nil
}

func readWendyDocs(ctx context.Context, name string, offset, limit int) (string, error) {
	if name == "" {
		var out strings.Builder
		err := fs.WalkDir(assets.FS, "docs", func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !entry.IsDir() && (strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".mdx")) {
				out.WriteString(strings.TrimPrefix(name, "docs/") + "\n")
			}
			return nil
		})
		return out.String(), err
	}
	name = strings.TrimPrefix(name, "docs/")
	if !fs.ValidPath(name) {
		return "", errors.New("documentation path must be a listed relative path")
	}
	file, err := assets.FS.Open(path.Join("docs", name))
	if err != nil {
		return "", err
	}
	defer file.Close()
	return readLines(ctx, file, offset, limit)
}

// limitedBuffer continues accepting writes after its capacity is reached, so
// noisy subprocesses cannot block on stdout/stderr or grow memory without bound.
type limitedBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := min(len(p), max(0, b.limit-len(b.data)))
	b.data = append(b.data, p[:n]...)
	b.truncated = b.truncated || n < len(p)
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := string(b.data)
	if b.truncated {
		result += "\n[Output truncated.]"
	}
	return result
}
