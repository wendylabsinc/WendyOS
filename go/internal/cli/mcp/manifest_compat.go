package mcp

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// These are published, reviewed catalogs whose older calls this CLI supports.
// Never download or accept a replacement catalog from the relay.
//
//go:embed manifests/*.json
var supportedRelayManifests embed.FS

// ResolveRelayManifest accepts the current catalog or a bundled older catalog
// that the current tools still implement. Version prefixes do not affect compatibility.
func ResolveRelayManifest(relayVersion string) (*ToolManifest, error) {
	current, err := Manifest()
	if err != nil {
		return nil, err
	}
	hash := manifestHash(relayVersion)
	if hash == manifestHash(current.Version) {
		return &current, nil
	}
	// Hashes are filenames only after validating the exact fixed-length encoding.
	if len(hash) != 16 {
		return nil, unsupportedRelayManifest(relayVersion, current.Version)
	}
	for _, c := range hash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, unsupportedRelayManifest(relayVersion, current.Version)
		}
	}
	raw, err := supportedRelayManifests.ReadFile("manifests/" + hash + ".json")
	if err != nil {
		return nil, unsupportedRelayManifest(relayVersion, current.Version)
	}
	var published ToolManifest
	if err := json.Unmarshal(raw, &published); err != nil {
		return nil, fmt.Errorf("reading supported relay manifest: %w", err)
	}
	if manifestHash(published.Version) != hash {
		return nil, fmt.Errorf("supported relay manifest has an unexpected version")
	}
	toolJSON, err := json.Marshal(published.Tools)
	if err != nil {
		return nil, fmt.Errorf("reading supported relay tools: %w", err)
	}
	sum := sha256.Sum256(toolJSON)
	if hex.EncodeToString(sum[:8]) != hash {
		return nil, fmt.Errorf("supported relay manifest does not match its published hash")
	}
	if err := compatibleRelayManifest(published, current); err != nil {
		return nil, fmt.Errorf("this CLI no longer supports relay manifest %q: %w", relayVersion, err)
	}
	return &published, nil
}

func unsupportedRelayManifest(relayVersion, currentVersion string) error {
	return fmt.Errorf("the relay publishes MCP tool manifest %q but this CLI serves %q and does not recognize that tool set; update the CLI or relay before pairing", relayVersion, currentVersion)
}

func toolDocument(tool mcpgo.Tool) (map[string]any, error) {
	raw, err := json.Marshal(tool)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	err = json.Unmarshal(raw, &doc)
	return doc, err
}

func compatibleRelayManifest(published, current ToolManifest) error {
	available := make(map[string]mcpgo.Tool, len(current.Tools))
	for _, tool := range current.Tools {
		available[tool.Name] = tool
	}
	for _, old := range published.Tools {
		tool, ok := available[old.Name]
		if !ok {
			return fmt.Errorf("tool %s is no longer available", old.Name)
		}
		a, err := toolDocument(old)
		if err != nil {
			return err
		}
		b, err := toolDocument(tool)
		if err != nil {
			return err
		}
		if !compatibleInputSchema(a["inputSchema"], b["inputSchema"]) {
			return fmt.Errorf("tool %s has an incompatible input schema", old.Name)
		}
		for _, doc := range []map[string]any{a, b} {
			delete(doc, "inputSchema")
			delete(doc, "description")
			delete(doc, "title")
			if annotations, ok := doc["annotations"].(map[string]any); ok {
				delete(annotations, "title")
			}
		}
		// Hints, defaults, output schemas and execution metadata are contracts too.
		// An older read-only hint must never conceal a newly mutating operation.
		if !reflect.DeepEqual(a, b) {
			return fmt.Errorf("tool %s changed its annotations or execution contract", old.Name)
		}
	}
	return nil
}

// Only proven additive schema changes are supported: optional top-level properties,
// expanded enums, and fewer required properties. All other constraints,
// including defaults, must stay identical. This is deliberately not a general
// JSON Schema implication solver; unfamiliar changes require an explicit migration.
func compatibleInputSchema(old, current any) bool {
	return compatibleSchema(old, current, true)
}

func compatibleSchema(old, current any, topLevel bool) bool {
	a, aOK := old.(map[string]any)
	b, bOK := current.(map[string]any)
	if !aOK || !bOK {
		return reflect.DeepEqual(old, current)
	}
	keys := map[string]bool{}
	for key := range a {
		keys[key] = true
	}
	for key := range b {
		keys[key] = true
	}
	for key := range keys {
		switch key {
		case "description", "title", "$comment":
			continue
		case "properties":
			previous, oldOK := a[key].(map[string]any)
			present, newOK := b[key].(map[string]any)
			if !oldOK || !newOK {
				return false
			}
			// Only top-level added properties are blocked by the dispatch guard.
			// Nested objects may intentionally be open maps; do not silently
			// assign new semantics to their previously unrestricted keys.
			if !topLevel && len(previous) != len(present) {
				return false
			}
			for name, schema := range previous {
				if next, ok := present[name]; !ok || !compatibleSchema(schema, next, false) {
					return false
				}
			}
			for name, schema := range present {
				if _, existed := previous[name]; !existed && schemaHasDefault(schema) {
					// Old callers cannot supply this property. A new default
					// could change their behavior even when they omit it.
					return false
				}
			}
		case "required":
			if !jsonSubset(b[key], a[key]) {
				return false
			}
		case "enum":
			if a[key] == nil || !jsonSubset(a[key], b[key]) {
				return false
			}
		case "items":
			if !compatibleSchema(a[key], b[key], false) {
				return false
			}
		default:
			if !reflect.DeepEqual(a[key], b[key]) {
				return false
			}
		}
	}
	return true
}

func schemaHasDefault(schema any) bool {
	switch value := schema.(type) {
	case map[string]any:
		if _, found := value["default"]; found {
			return true
		}
		for _, nested := range value {
			if schemaHasDefault(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range value {
			if schemaHasDefault(nested) {
				return true
			}
		}
	}
	return false
}

func jsonSubset(subset, superset any) bool {
	a, ok := subset.([]any)
	if !ok {
		return subset == nil
	}
	b, ok := superset.([]any)
	if !ok {
		return len(a) == 0 && superset == nil
	}
	for _, value := range a {
		found := false
		for _, other := range b {
			if reflect.DeepEqual(value, other) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

type relayManifestTool struct {
	tool   mcpgo.Tool
	schema *jsonschema.Schema
}

type relayManifestProfile struct {
	tools map[string]*relayManifestTool
}

// No remote schema references: a relay tool schema must not turn compilation
// into an HTTP request or filesystem read.
type noRemoteSchemaLoader struct{}

func (noRemoteSchemaLoader) Load(string) (any, error) {
	return nil, fmt.Errorf("external schema references are not supported")
}

func prepareRelayManifest(version string) (*relayManifestProfile, error) {
	manifest, err := ResolveRelayManifest(version)
	if err != nil {
		return nil, err
	}
	profile := &relayManifestProfile{tools: make(map[string]*relayManifestTool, len(manifest.Tools))}
	for _, tool := range manifest.Tools {
		doc, err := toolDocument(tool)
		if err != nil {
			return nil, err
		}
		compiler := jsonschema.NewCompiler()
		compiler.UseLoader(noRemoteSchemaLoader{})
		const schemaURI = "urn:wendy:relay-tool"
		if err := compiler.AddResource(schemaURI, doc["inputSchema"]); err != nil {
			return nil, err
		}
		schema, err := compiler.Compile(schemaURI)
		if err != nil {
			return nil, fmt.Errorf("relay tool %s schema: %w", tool.Name, err)
		}
		profile.tools[tool.Name] = &relayManifestTool{tool: tool, schema: schema}
	}
	return profile, nil
}

func (s *mcpServer) filterManifestTools(ctx context.Context, tools []mcpgo.Tool) []mcpgo.Tool {
	tools = s.filterTools(ctx, tools)
	if !s.relay || s.relayProfile == nil {
		return tools
	}
	visible := make([]mcpgo.Tool, 0, len(tools))
	for _, tool := range tools {
		if published := s.relayProfile.tools[tool.Name]; published != nil {
			visible = append(visible, published.tool)
		}
	}
	return visible
}
