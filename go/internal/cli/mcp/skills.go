package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/assets"
	"gopkg.in/yaml.v3"
)

const skillsExtension = "io.modelcontextprotocol/skills"
const skillBaseURI = "skill://wendy/wendy/"

type skillResource struct {
	URI    string `json:"uri"`
	Digest string `json:"digest"`
}

type skillEntry struct {
	URI         string          `json:"uri"`
	Frontmatter map[string]any  `json:"frontmatter"`
	Resources   []skillResource `json:"resources"`
}

type skillCatalog struct {
	Skills []skillEntry
	files  map[string][]byte
}

// The catalog and its bytes are immutable for the lifetime of the binary.
var embeddedSkillCatalog = sync.OnceValues(buildSkillCatalog)

// The sync script packages the end-user group as one skill to fit OpenAI's
// five-skill import limit. Read only that generated bundle, never user files.
func buildSkillCatalog() (*skillCatalog, error) {
	const root = "mcp-skills/wendy"
	main, err := assets.FS.ReadFile(root + "/SKILL.md")
	if err != nil {
		return nil, err
	}
	frontmatter, _, err := skillFrontmatter(main)
	if err != nil {
		return nil, err
	}
	if frontmatter["name"] != "wendy" {
		return nil, fmt.Errorf("MCP skill name must match its directory")
	}
	files := map[string][]byte{}
	if err := fs.WalkDir(assets.FS, root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := assets.FS.ReadFile(p)
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(p, root+"/")] = data
		return nil
	}); err != nil {
		return nil, err
	}
	entry := skillEntry{URI: skillBaseURI + "SKILL.md", Frontmatter: frontmatter}
	catalog := &skillCatalog{files: make(map[string][]byte, len(files))}
	var paths []string
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	var total int
	seen := map[string]bool{}
	for _, p := range paths {
		// Reject ambiguous paths rather than normalizing them into another file.
		folded := strings.ToLower(p)
		if !fs.ValidPath(p) || strings.ContainsAny(p, "\\%?#:") || seen[folded] {
			return nil, fmt.Errorf("unsafe or duplicate skill resource %q", p)
		}
		seen[folded] = true
		data := files[p]
		limit := 1 << 20
		if p == "SKILL.md" {
			limit = 256 << 10
		}
		if len(data) > limit {
			return nil, fmt.Errorf("skill resource %s exceeds import limit", p)
		}
		total += len(data)
		uri := skillBaseURI + p
		entry.Resources = append(entry.Resources, skillResource{URI: uri, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(data))})
		catalog.files[uri] = data
	}
	if len(files) > 100 || total > 5<<20 {
		return nil, fmt.Errorf("bundled Wendy skill exceeds import limits")
	}
	catalog.Skills = []skillEntry{entry}
	return catalog, nil
}

// Decode the whole YAML mapping, not just name/description. The importer checks
// that the catalog contains every frontmatter field from the returned SKILL.md.
func skillFrontmatter(data []byte) (map[string]any, int, error) {
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return nil, 0, fmt.Errorf("missing YAML frontmatter")
	}
	end := bytes.Index(data[4:], []byte("\n---\n"))
	if end < 0 {
		return nil, 0, fmt.Errorf("unterminated YAML frontmatter")
	}
	var fm map[string]any
	if err := yaml.Unmarshal(data[4:4+end], &fm); err != nil {
		return nil, 0, err
	}
	for _, key := range []string{"name", "description"} {
		value, ok := fm[key].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nil, 0, fmt.Errorf("frontmatter requires %s", key)
		}
	}
	// Normalize YAML numeric and nested values to their JSON representation.
	raw, err := json.Marshal(fm)
	if err != nil {
		return nil, 0, err
	}
	if err := json.Unmarshal(raw, &fm); err != nil {
		return nil, 0, err
	}
	return fm, 4 + end + len("\n---\n"), nil
}

func registerSkills(srv *server.MCPServer) error {
	catalog, err := embeddedSkillCatalog()
	if err != nil {
		return fmt.Errorf("loading MCP skills: %w", err)
	}
	hooks := srv.GetHooks()
	if hooks == nil {
		hooks = &server.Hooks{}
		server.WithHooks(hooks)(srv)
	}
	hooks.AddAfterInitialize(func(_ context.Context, _ any, _ *mcpgo.InitializeRequest, result *mcpgo.InitializeResult) {
		if result.Capabilities.Extensions == nil {
			result.Capabilities.Extensions = map[string]any{}
		}
		result.Capabilities.Extensions[skillsExtension] = map[string]any{}
	})
	for _, skill := range catalog.Skills {
		for _, resource := range skill.Resources {
			uri := resource.URI
			data := catalog.files[uri]
			mimeType := "application/octet-stream"
			if utf8.Valid(data) {
				mimeType = "text/plain; charset=utf-8"
				if path.Ext(uri) == ".md" {
					mimeType = "text/markdown; charset=utf-8"
				}
			}
			srv.AddResource(mcpgo.NewResource(uri, strings.TrimPrefix(uri, skillBaseURI), mcpgo.WithMIMEType(mimeType)), func(_ context.Context, _ mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
				if utf8.Valid(data) {
					return []mcpgo.ResourceContents{mcpgo.TextResourceContents{URI: uri, MIMEType: mimeType, Text: string(data)}}, nil
				}
				return []mcpgo.ResourceContents{mcpgo.BlobResourceContents{URI: uri, MIMEType: mimeType, Blob: base64.StdEncoding.EncodeToString(data)}}, nil
			})
		}
	}
	return nil
}

type skillRPCResponse struct {
	JSONRPC string                     `json:"jsonrpc"`
	ID      json.RawMessage            `json:"id"`
	Result  any                        `json:"result,omitempty"`
	Error   *mcpgo.JSONRPCErrorDetails `json:"error,omitempty"`
}

// The SDK does not dispatch draft extension methods. Intercept only these two
// methods at each transport boundary and let the SDK handle all standard MCP.
func (c *skillCatalog) dispatch(raw []byte) (*skillRPCResponse, bool) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if json.Unmarshal(raw, &req) != nil || (req.Method != "skills/list" && req.Method != "skills/get") {
		return nil, false
	}
	response := &skillRPCResponse{JSONRPC: "2.0", ID: req.ID}
	fail := func(code int, message string) (*skillRPCResponse, bool) {
		response.Error = &mcpgo.JSONRPCErrorDetails{Code: code, Message: message}
		return response, true
	}
	if len(req.ID) == 0 || bytes.Equal(req.ID, []byte("null")) {
		return nil, true // Notifications never receive a response.
	}
	if req.ID[0] != '"' && req.ID[0] != '-' && (req.ID[0] < '0' || req.ID[0] > '9') {
		response.ID = json.RawMessage("null")
		return fail(mcpgo.INVALID_REQUEST, "Invalid request ID")
	}
	if req.JSONRPC != "2.0" {
		return fail(mcpgo.INVALID_REQUEST, "Invalid JSON-RPC version")
	}
	var params map[string]json.RawMessage
	if len(req.Params) > 0 && (bytes.Equal(req.Params, []byte("null")) || json.Unmarshal(req.Params, &params) != nil) {
		return fail(mcpgo.INVALID_PARAMS, "Expected an object of skill parameters")
	}
	switch req.Method {
	case "skills/list":
		if raw, ok := params["cursor"]; ok {
			var cursor string
			if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &cursor) != nil || cursor != "" {
				return fail(mcpgo.INVALID_PARAMS, "Invalid skills cursor; request the first page with {}")
			}
		}
		// A single terminal page. Omit nextCursor rather than issuing a cursor
		// that could repeat or skip entries in this static catalog.
		response.Result = struct {
			Skills []skillEntry `json:"skills"`
		}{c.Skills}
	case "skills/get":
		var uri string
		if json.Unmarshal(params["uri"], &uri) != nil || uri == "" {
			return fail(mcpgo.INVALID_PARAMS, "A catalog SKILL.md URI is required")
		}
		for _, skill := range c.Skills {
			if skill.URI == uri {
				response.Result = struct {
					Skill skillEntry `json:"skill"`
				}{skill}
				return response, true
			}
		}
		return fail(mcpgo.INVALID_PARAMS, "Unknown skill URI")
	}
	return response, true
}
