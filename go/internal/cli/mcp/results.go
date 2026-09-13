package mcp

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// okResult returns a success result carrying v as structuredContent plus an
// indented-JSON text fallback for hosts that do not render structured content.
func okResult(v any) *mcpgo.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errResultf(errCodeInternal, "marshaling result: %s", err.Error())
	}
	// MCP requires structuredContent to be a JSON object. Wrap anything that is
	// not one (array, scalar, null) rather than emitting a result conformant
	// clients reject outright. Recurses at most once: a map[string]any always
	// marshals to an object. Prefer okList at call sites — it gives the payload
	// a meaningful key instead of the generic one used here.
	if len(b) == 0 || b[0] != '{' {
		return okResult(map[string]any{"result": v})
	}
	return mcpgo.NewToolResultStructured(v, string(b))
}

// okList wraps a list payload under key in an object envelope. MCP requires
// structuredContent to be a JSON object, so returning a bare array makes
// conformant clients reject the whole result. A nil slice is normalized to []
// so the key is always present.
func okList[T any](key string, items []T) *mcpgo.CallToolResult {
	return okResult(map[string]any{key: listOrEmpty(items)})
}

// okListBounded is okList with okResultBounded's byte ceiling on the payload.
func okListBounded[T any](key string, items []T, maxBytes int) *mcpgo.CallToolResult {
	return okResultBounded(map[string]any{key: listOrEmpty(items)}, maxBytes)
}

// listOrEmpty returns items, substituting an empty slice for nil so it
// marshals as [] rather than null.
func listOrEmpty[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

// okText returns a plain-text success result (for simple confirmations).
func okText(msg string) *mcpgo.CallToolResult {
	return mcpgo.NewToolResultText(msg)
}

// okResultBounded is okResult with a byte ceiling on the JSON text fallback.
// gRPC streams are not resumable, so when the payload exceeds maxBytes we do
// not paginate — we return a truncation envelope telling the agent to narrow
// the query. maxBytes <= 0 disables the cap (behaves as okResult).
func okResultBounded(v any, maxBytes int) *mcpgo.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errResultf(errCodeInternal, "marshaling result: %s", err.Error())
	}
	if maxBytes > 0 && len(b) > maxBytes {
		env := map[string]any{
			"truncated": true,
			"max_bytes": maxBytes,
			"bytes":     len(b),
			"note":      "output exceeded max_bytes; narrow the query (reduce max_batches / max_chunks, add filters, or raise max_bytes)",
		}
		eb, _ := json.MarshalIndent(env, "", "  ")
		return mcpgo.NewToolResultStructured(env, string(eb))
	}
	// Delegate so the under-budget path gets okResult's object guard too.
	return okResult(v)
}

// okTextBounded returns a plain-text success result like okText, but clamps s
// to maxBytes bytes and appends a truncation note when it exceeds the cap. The
// cut is backed off to the nearest UTF-8 rune boundary so the result is never
// invalid UTF-8 (which some hosts reject when serializing TextContent). hint is
// a tool-specific suggestion for narrowing output; a generic one is used when
// empty. Kept separate from okResultBounded because JSON-marshaling a plain
// string (as okResultBounded would) quotes and escapes it, changing the text
// output format for tools whose callers rely on the raw, unquoted text.
// maxBytes <= 0 disables the cap (behaves as okText).
func okTextBounded(s, hint string, maxBytes int) *mcpgo.CallToolResult {
	if maxBytes > 0 && len(s) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if hint == "" {
			hint = "narrow the query (reduce max_chunks, add filters, or raise max_bytes)"
		}
		s = s[:cut] + fmt.Sprintf("\n[truncated: output exceeded max_bytes=%d; %s]", maxBytes, hint)
	}
	return okText(s)
}

// defaultProxyMaxBytes bounds the size of a result proxied in from a
// container's own MCP server (see addProxiedTools in app_tools.go).
// Unlike wendy's native tools, a container-supplied tool's output shape is
// not under our control — a poorly-behaved or malicious app could return an
// arbitrarily large payload (e.g. dumping a huge file or log) straight into
// the calling LLM's context. This mirrors the 100000-byte default the native
// tools pass to okResultBounded/okTextBounded (see tools_container.go,
// tools_cloud.go, tools_telemetry.go) so proxied tools get the same
// truncation discipline.
const defaultProxyMaxBytes = 100000

// capProxiedResult enforces maxBytes on a *mcpgo.CallToolResult returned by a
// proxied container MCP tool. If the serialized result fits under the cap it
// is returned unchanged (same pointer); otherwise its content is replaced
// with the most informative truncation envelope that itself fits under the
// cap. IsError is preserved so error results still surface as errors, just
// with bounded content. maxBytes <= 0 disables the cap. A nil result is passed
// through untouched.
//
// The MCP result wire format has an unavoidable minimum size (the content
// array plus isError for errors). If maxBytes is below that floor, the smallest
// valid result is returned even though no valid CallToolResult can fit.
func capProxiedResult(result *mcpgo.CallToolResult, maxBytes int) *mcpgo.CallToolResult {
	if result == nil || maxBytes <= 0 {
		return result
	}
	if !proxiedResultExceedsMaxBytes(result, maxBytes) {
		return result
	}

	// Try progressively smaller envelopes so low test/configuration limits do
	// not make the truncation response larger than the payload ceiling.
	envelopes := []map[string]any{
		{
			"truncated": true,
			"max_bytes": maxBytes,
			"note":      "proxied container tool output exceeded max_bytes; ask the app's tool for a narrower result",
		},
		{"truncated": true, "max_bytes": maxBytes},
		{"truncated": true},
	}
	for _, env := range envelopes {
		text, _ := json.Marshal(env)
		bounded := &mcpgo.CallToolResult{
			Content:           []mcpgo.Content{mcpgo.NewTextContent(string(text))},
			StructuredContent: env,
			IsError:           result.IsError,
		}
		if proxiedResultFitsMaxBytes(bounded, maxBytes) {
			return bounded
		}
	}

	// Structured content duplicates its text fallback on the wire. Drop it
	// before dropping the truncation signal altogether.
	bounded := &mcpgo.CallToolResult{
		Content: []mcpgo.Content{mcpgo.NewTextContent("truncated")},
		IsError: result.IsError,
	}
	if proxiedResultFitsMaxBytes(bounded, maxBytes) {
		return bounded
	}

	// This is the smallest protocol-valid result. It is only over budget when
	// maxBytes is below the MCP wire-format floor described above.
	return &mcpgo.CallToolResult{Content: []mcpgo.Content{}, IsError: result.IsError}
}

// proxiedResultFitsMaxBytes only marshals already-bounded replacement
// candidates. The untrusted original is measured by the allocation-bounded
// sizer in proxied_result_size.go.
func proxiedResultFitsMaxBytes(result *mcpgo.CallToolResult, maxBytes int) bool {
	b, err := json.Marshal(result)
	return err == nil && len(b) <= maxBytes
}
