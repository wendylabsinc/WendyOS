package mcp

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/assets"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// toolReferenceAllowlist names snake_case words in agent-facing text that
// look like Wendy MCP tools but are not, each with the reason it may stay.
var toolReferenceAllowlist = map[string]string{
	"wendy_docs":              "Wendy chat built-in tool, named by the chat prompt",
	"camera_view":             "Wendy chat built-in tool, named by the chat prompt",
	"audio_listen":            "Wendy chat built-in tool, named by the chat prompt",
	"background_process_list": "Wendy chat built-in tool, named by the chat prompt",
	"background_process_stop": "Wendy chat built-in tool, named by the chat prompt",
	"memory_search":           "Wendy chat built-in tool, named by the chat prompt",
	"memory_save":             "Wendy chat built-in tool, named by the chat prompt",
	"container_attach":        "retired alias: its handler stays for old callers but is never advertised; texts name it to steer agents to container_exec",
	"deploy_app":              "MCP prompt (prompts.go), not a tool",
	"cloud_deskewed":          "part of the example ROS topic /utlidar/cloud_deskewed",
	"read_export":             "the wendy-data example script read_export.py",
	"device_campaign_sources": "native Wendy chat tool from the macOS app backend, named by the wendy-data skill",
	"device_campaign_list":    "native Wendy chat tool from the macOS app backend, named by the wendy-data skill",
	"job_dir":                 "a {job_dir} step placeholder in the wendy-training skill",
	"app_id":                  "the Cloud app ID field in wendy.json.md",
}

var (
	toolReferencePattern  = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`)
	groupReferencePattern = regexp.MustCompile(`groups=\[([^\]]*)\]`)
	quotedNamePattern     = regexp.MustCompile(`["']([^"']*)["']`)
)

// agentFacingSources returns every text Wendy ships to agents, keyed by a
// label for error messages: the MCP and relay instructions, the guide, the
// chat prompt, the end-user and MCP skill bundles, and AGENTS.md.
func agentFacingSources(t *testing.T) map[string]string {
	t.Helper()
	sources := maps.Clone(AgentFacingTexts())
	sources["chat prompt"] = chat.SystemPrompt("", "")
	names, err := assets.EndUserSkillNames()
	if err != nil {
		t.Fatal(err)
	}
	roots := []string{"mcp-skills"}
	for _, name := range names {
		roots = append(roots, "skills/"+name)
	}
	for _, root := range roots {
		err := fs.WalkDir(assets.FS, root, func(name string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(name, ".md") {
				return err
			}
			data, err := assets.FS.ReadFile(name)
			sources[name] = string(data)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile("../../../../AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	sources["AGENTS.md"] = string(data)
	return sources
}

type toolCatalog struct {
	tools, params, prefixes map[string]bool
}

// realToolCatalog lists the tools an agent can discover: every built-in tool
// tools/list shows with all groups enabled. A registered handler that is never advertised (container_attach) is not a
// tool an agent should be told to call.
func realToolCatalog(t *testing.T) toolCatalog {
	t.Helper()
	s := New(&config.Config{}, nil)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetToolGroups([]string{"all"}); err != nil {
		t.Fatal(err)
	}
	c := toolCatalog{tools: map[string]bool{}, params: map[string]bool{}, prefixes: map[string]bool{}}
	var builtin []mcpgo.Tool
	for _, entry := range srv.ListTools() {
		builtin = append(builtin, entry.Tool)
	}
	advertised := s.filterTools(context.Background(), builtin)
	for _, tool := range advertised {
		c.tools[tool.Name] = true
		if prefix, _, found := strings.Cut(tool.Name, "_"); found {
			c.prefixes[prefix] = true
		}
	}
	for _, tool := range append(builtin, advertised...) {
		for param := range tool.InputSchema.Properties {
			c.params[param] = true
		}
	}
	return c
}

// toolReferences returns the snake_case words in text that share a prefix
// with a real tool and are not a tool parameter, so read as tool names.
func (c toolCatalog) toolReferences(text string) []string {
	var refs []string
	for _, word := range toolReferencePattern.FindAllString(text, -1) {
		prefix, _, _ := strings.Cut(word, "_")
		if c.prefixes[prefix] && !c.params[word] && !slices.Contains(refs, word) {
			refs = append(refs, word)
		}
	}
	return refs
}

// Agents learn Wendy from the text we ship. A tool or group named there that
// no longer exists sends them the wrong way.
func TestAgentFacingTextNamesOnlyRealTools(t *testing.T) {
	catalog := realToolCatalog(t)
	sources := agentFacingSources(t)
	checked := 0
	for _, label := range slices.Sorted(maps.Keys(sources)) {
		text := sources[label]
		for _, name := range catalog.toolReferences(text) {
			checked++
			if _, allowed := toolReferenceAllowlist[name]; !catalog.tools[name] && !allowed {
				t.Errorf("%s: %s is not a tool; fix the text or add an allowlist entry with a reason", label, name)
			}
		}
		seen := map[string]bool{}
		for _, match := range groupReferencePattern.FindAllStringSubmatch(text, -1) {
			for _, quoted := range quotedNamePattern.FindAllStringSubmatch(match[1], -1) {
				group := quoted[1]
				if !slices.Contains(toolGroupNames, group) && !seen[group] {
					seen[group] = true
					t.Errorf("%s: %s is not a tool group; fix the text or add an allowlist entry with a reason", label, group)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no tool references in agent-facing text; the matcher is broken")
	}
}

func TestToolReferenceAllowlistIsStillNeeded(t *testing.T) {
	catalog := realToolCatalog(t)
	mentioned := map[string]bool{}
	for _, text := range agentFacingSources(t) {
		for _, word := range toolReferencePattern.FindAllString(text, -1) {
			mentioned[word] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(toolReferenceAllowlist)) {
		if catalog.tools[name] {
			t.Errorf("allowlist entry %s is a real tool; remove it", name)
		}
		if !mentioned[name] {
			t.Errorf("allowlist entry %s appears in no agent-facing text; remove it", name)
		}
		if strings.TrimSpace(toolReferenceAllowlist[name]) == "" {
			t.Errorf("allowlist entry %s has no reason", name)
		}
	}
}
