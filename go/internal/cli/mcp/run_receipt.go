package mcp

import (
	"encoding/json"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// The child writes its receipt on stdout and progress on stderr. Only a whole,
// valid stdout document is a receipt: JSON mentioned inside build logs is not.
type runReceipt struct {
	Status            string          `json:"status"`
	App               string          `json:"app,omitempty"`
	Readiness         string          `json:"readiness"`
	URL               string          `json:"url,omitempty"`
	Endpoints         json.RawMessage `json:"endpoints,omitempty"`
	ExitCode          *int32          `json:"exit_code,omitempty"`
	TerminationReason string          `json:"termination_reason,omitempty"`
	Message           string          `json:"message,omitempty"`
}

type runReceiptEnvelope struct {
	Schema  string      `json:"schema"`
	Receipt *runReceipt `json:"receipt"`
	Output  string      `json:"output"`
}

func parseRunReceipt(data []byte) *runReceipt {
	var receipt runReceipt
	if json.Unmarshal(data, &receipt) != nil {
		return nil
	}
	switch receipt.Status {
	case "started", "created", "running", "ready", "crashed", "not_ready", "failed":
	default:
		return nil
	}
	switch receipt.Readiness {
	case "not_checked", "passed", "failed":
	default:
		return nil
	}
	if (receipt.Status == "ready") != (receipt.Readiness == "passed") {
		return nil
	}
	if receipt.Status != "failed" && appconfig.ValidateAppID(receipt.App) != nil {
		return nil
	}
	if (receipt.Status == "started" || receipt.Status == "created" || receipt.Status == "running" || receipt.Status == "failed") && receipt.Readiness != "not_checked" {
		return nil
	}
	return &receipt
}

func encodeRunReceipt(stdout []byte, output string) string {
	receipt := parseRunReceipt(stdout)
	if receipt == nil {
		return output
	}
	encoded, _ := json.Marshal(runReceiptEnvelope{Schema: "wendy.mcp.run.v1", Receipt: receipt, Output: output})
	return string(encoded)
}

func decodeRunReceipt(output string) (*runReceipt, string) {
	var envelope runReceiptEnvelope
	if json.Unmarshal([]byte(output), &envelope) != nil || envelope.Schema != "wendy.mcp.run.v1" || envelope.Receipt == nil {
		return nil, output
	}
	encoded, _ := json.Marshal(envelope.Receipt)
	return parseRunReceipt(encoded), envelope.Output
}

func copyRunReceipt(result map[string]any, receipt *runReceipt) {
	encoded, _ := json.Marshal(receipt)
	var fields map[string]any
	_ = json.Unmarshal(encoded, &fields)
	for name, value := range fields {
		result[name] = value
	}
}

func runBoolean(req mcpgo.CallToolRequest, name string, defaultValue bool) (bool, error) {
	value, exists := req.GetArguments()[name]
	if !exists {
		return defaultValue, nil
	}
	flag, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return flag, nil
}
