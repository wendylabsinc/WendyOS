package chat

import (
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"testing"
)

func TestMCPCommandOutcomeMetadataDoesNotGuessFromErrorText(t *testing.T) {
	for _, marker := range []bool{false, true} {
		r := mcpgo.NewToolResultStructured(map[string]any{"command_not_started": marker}, "[INVALID_ARGUMENT] failed")
		r.IsError = true
		got, err := mediaToolResult(r)
		if err == nil || got.CommandNotStarted != marker {
			t.Fatalf("outcome lost: %+v %v", got, err)
		}
		r.IsError = false
		got, err = mediaToolResult(r)
		if err != nil || got.CommandNotStarted {
			t.Fatalf("success acquired failure metadata: %+v %v", got, err)
		}
	}
}
