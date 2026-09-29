package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestOkResult_HasStructuredAndJSONText(t *testing.T) {
	r := okResult(map[string]any{"version": "1.2.3"})
	if r.IsError {
		t.Fatal("expected success result")
	}
	if r.StructuredContent == nil {
		t.Fatal("expected structured content")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(toolResultText(t, r)), &m); err != nil {
		t.Fatalf("text fallback is not valid JSON: %v", err)
	}
	if m["version"] != "1.2.3" {
		t.Errorf("version = %v", m["version"])
	}
}

// MCP requires structuredContent to be a JSON object. A bare array made every
// list-shaped tool fail client-side schema validation (WDY-2001), so okResult
// wraps any non-object payload rather than emitting one.
func TestOkResult_WrapsNonObjectPayload(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    any
	}{
		{"slice", []map[string]any{{"a": 1}}},
		{"empty slice", []map[string]any{}},
		{"nil slice", []map[string]any(nil)},
		{"scalar", "hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := okResult(tc.v)
			if _, ok := r.StructuredContent.(map[string]any); !ok {
				t.Fatalf("structuredContent must be an object, got %T", r.StructuredContent)
			}
			text := toolResultText(t, r)
			if text[0] != '{' {
				t.Errorf("text fallback must be a JSON object, got %s", text)
			}
		})
	}
}

func TestOkList_WrapsUnderKeyAndNormalizesNil(t *testing.T) {
	r := okList("devices", []map[string]any(nil))
	sc, ok := r.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected object structured content, got %T", r.StructuredContent)
	}
	// The key must be present with an empty list, not absent and not null —
	// agents branch on it without a nil check.
	var env map[string][]map[string]any
	if err := json.Unmarshal([]byte(toolResultText(t, r)), &env); err != nil {
		t.Fatalf("text fallback is not valid JSON: %v", err)
	}
	rows, ok := env["devices"]
	if !ok {
		t.Fatal("envelope is missing the devices key")
	}
	if len(rows) != 0 {
		t.Errorf("expected empty list, got %v", rows)
	}
	if _, ok := sc["devices"]; !ok {
		t.Error("structuredContent is missing the devices key")
	}
}

func TestOkListBounded_TruncationEnvelopeSurvives(t *testing.T) {
	big := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		big = append(big, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	}
	r := okListBounded("batches", big, 200)
	sc, ok := r.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected truncation envelope map, got %T", r.StructuredContent)
	}
	if sc["truncated"] != true {
		t.Errorf("expected truncated=true, got %v", sc["truncated"])
	}
}

func TestOkResultBounded_TruncatesOversize(t *testing.T) {
	big := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		big = append(big, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	}
	r := okResultBounded(big, 200)
	if r.IsError {
		t.Fatal("truncation is not an error result")
	}
	sc, ok := r.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected truncation envelope map, got %T", r.StructuredContent)
	}
	if sc["truncated"] != true {
		t.Errorf("expected truncated=true, got %v", sc["truncated"])
	}
}

func TestOkResultBounded_PassesSmall(t *testing.T) {
	r := okResultBounded(map[string]any{"k": "v"}, 100000)
	sc, _ := r.StructuredContent.(map[string]any)
	if sc["truncated"] == true {
		t.Error("small payload should not be truncated")
	}
}

func TestOkTextBounded_TruncatesOnRuneBoundary(t *testing.T) {
	// A string of 3-byte runes (each "世" is 3 bytes). Cutting at a byte cap
	// that lands mid-rune must back off so the result stays valid UTF-8.
	s := strings.Repeat("世", 20)  // 60 bytes
	r := okTextBounded(s, "", 10) // 10 is not a multiple of 3 → mid-rune cut
	text := toolResultText(t, r)
	if !utf8.ValidString(text) {
		t.Fatalf("truncated output is not valid UTF-8: %q", text)
	}
	if !strings.Contains(text, "truncated") {
		t.Error("expected a truncation note in the output")
	}
}

func TestOkTextBounded_PassesSmall(t *testing.T) {
	r := okTextBounded("hello", "", 100000)
	if toolResultText(t, r) != "hello" {
		t.Error("under-cap text should be returned verbatim")
	}
}

// A poorly-behaved or malicious container-supplied MCP tool could return an
// arbitrarily large payload; capProxiedResult must bound it the same way
// okResultBounded/okTextBounded bound wendy's own tools.
func TestCapProxiedResult_TruncatesOversize(t *testing.T) {
	big := mcpgo.NewToolResultText(strings.Repeat("a", 1000))
	r := capProxiedResult(big, 200)
	if r == big {
		t.Fatal("expected a new, bounded result, got the original pointer back")
	}
	sc, ok := r.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected truncation envelope map, got %T", r.StructuredContent)
	}
	if sc["truncated"] != true {
		t.Errorf("expected truncated=true, got %v", sc["truncated"])
	}
	if !strings.Contains(toolResultText(t, r), "truncated") {
		t.Error("expected a truncation note in the text fallback")
	}
}

func TestCapProxiedResult_TruncationResultFitsSmallCaps(t *testing.T) {
	for _, maxBytes := range []int{32, 64, 100, 200} {
		t.Run(fmt.Sprint(maxBytes), func(t *testing.T) {
			big := mcpgo.NewToolResultText(strings.Repeat("a", 1000))
			r := capProxiedResult(big, maxBytes)
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatalf("marshal bounded result: %v", err)
			}
			if len(b) > maxBytes {
				t.Fatalf("bounded result is %d bytes, want <= %d: %s", len(b), maxBytes, b)
			}
		})
	}
}

func TestProxiedResultSizerMatchesWireJSON(t *testing.T) {
	priority := 0.5
	results := []*mcpgo.CallToolResult{
		mcpgo.NewToolResultText("hello"),
		mcpgo.NewToolResultText("<escaped> \"text\"\n\u2028"),
		mcpgo.NewToolResultStructured(
			map[string]any{"nested": []any{"value", float64(42), float64(1e-9), float32(1e21), true, nil}},
			`{"nested":["value",42,1e-9,1e21,true,null]}`,
		),
		mcpgo.NewToolResultError("failed"),
		{
			Result: mcpgo.Result{Meta: &mcpgo.Meta{
				ProgressToken: "progress-1",
				AdditionalFields: map[string]any{
					"source": "container",
				},
			}},
			Content: []mcpgo.Content{
				mcpgo.TextContent{
					Annotated: mcpgo.Annotated{Annotations: &mcpgo.Annotations{
						Audience: []mcpgo.Role{mcpgo.RoleUser},
						Priority: &priority,
					}},
					Type: "text",
					Text: "annotated",
				},
				mcpgo.ImageContent{Type: "image", Data: "aGVsbG8=", MIMEType: "image/png"},
				mcpgo.AudioContent{Type: "audio", Data: "aGVsbG8=", MIMEType: "audio/wav"},
				mcpgo.ResourceLink{Type: "resource_link", URI: "https://example.com", Name: "example"},
				mcpgo.EmbeddedResource{
					Type: "resource",
					Resource: mcpgo.TextResourceContents{
						URI:      "file:///tmp/example.txt",
						MIMEType: "text/plain",
						Text:     "resource text",
					},
				},
				mcpgo.ToolUseContent{
					Type:  "tool_use",
					ID:    "tool-1",
					Name:  "example",
					Input: map[string]any{"value": "input"},
				},
				mcpgo.ToolResultContent{
					Type:      "tool_result",
					ToolUseID: "tool-1",
					Content:   []mcpgo.Content{mcpgo.NewTextContent("nested")},
				},
			},
			StructuredContent: map[string]any{"ok": true},
			IsError:           true,
		},
	}

	for i, result := range results {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			b, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			if proxiedResultExceedsMaxBytes(result, len(b)) {
				t.Fatalf("exact-size result reported over limit: size=%d", len(b))
			}
			if !proxiedResultExceedsMaxBytes(result, len(b)-1) {
				t.Fatalf("result not reported over one-byte-short limit: size=%d", len(b))
			}
		})
	}
}

// Oversized error results (isError=true) must still surface as errors after
// truncation — bounding output size is not license to swallow the error flag.
func TestCapProxiedResult_PreservesIsErrorOnTruncation(t *testing.T) {
	big := mcpgo.NewToolResultError(strings.Repeat("e", 1000))
	r := capProxiedResult(big, 200)
	if !r.IsError {
		t.Error("expected IsError to survive truncation")
	}
}

func TestCapProxiedResult_PassesSmallUnchanged(t *testing.T) {
	small := mcpgo.NewToolResultText("hello")
	r := capProxiedResult(small, defaultProxyMaxBytes)
	if r != small {
		t.Fatal("expected the original result to be returned unchanged for a small payload")
	}
}

func TestCapProxiedResult_NilPassesThrough(t *testing.T) {
	if r := capProxiedResult(nil, defaultProxyMaxBytes); r != nil {
		t.Fatalf("expected nil to pass through, got %v", r)
	}
}

func TestCapProxiedResult_ZeroMaxBytesDisablesCap(t *testing.T) {
	big := mcpgo.NewToolResultText(strings.Repeat("a", 1000))
	r := capProxiedResult(big, 0)
	if r != big {
		t.Fatal("maxBytes<=0 should disable the cap and return the original result")
	}
}

func TestOkListBounded_KeepsCompleteRowsAndCountsOmissions(t *testing.T) {
	items := make([]map[string]any, 0, 20)
	for i := range 20 {
		items = append(items, map[string]any{"id": i, "body": strings.Repeat("世", 40)})
	}
	r := okListBounded("logs", items, 512)
	rows := listPayload(t, r, "logs")
	if len(rows) == 0 || len(rows) >= len(items) || len(toolResultText(t, r)) > 512 {
		t.Fatalf("want a non-empty partial list within 512 bytes, got %d rows: %s", len(rows), toolResultText(t, r))
	}
	meta := structuredMap(t, r)
	if meta["truncated"] != true || meta["returned"] != len(rows) || meta["omitted"] != len(items)-len(rows) {
		t.Fatalf("bad truncation metadata: %v", meta)
	}
	if rows[0]["body"] != items[0]["body"] || rows[len(rows)-1]["id"] != float64(len(rows)-1) {
		t.Fatal("rows must be a complete, in-order prefix")
	}
}

// Review focus: one row larger than the budget yields an explicit empty
// prefix with omitted counts, never a silently empty list.
func TestOkListBounded_SingleOversizedRowReportsOmission(t *testing.T) {
	r := okListBounded("batches", []string{strings.Repeat("a", 1000)}, 200)
	meta := structuredMap(t, r)
	if meta["truncated"] != true || meta["returned"] != 0 || meta["omitted"] != 1 {
		t.Fatalf("oversized row must be reported as omitted: %v", meta)
	}
	if rows := listPayload(t, r, "batches"); len(rows) != 0 {
		t.Fatalf("rows = %v", rows)
	}
}
