package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

type telemetryOptionsValue struct {
	format                           string
	maxBytes, maxRecords, maxBatches int
}

func telemetryOptions() []mcpgo.ToolOption {
	return []mcpgo.ToolOption{
		mcpgo.WithString("format", mcpgo.Enum("compact", "otlp"), mcpgo.DefaultString("compact"), mcpgo.Description("Compact records or original OTLP batches")),
		mcpgo.WithInteger("max_records", mcpgo.Min(1), mcpgo.Max(1000), mcpgo.DefaultNumber(100), mcpgo.Description("Compact record limit")),
		mcpgo.WithInteger("max_batches", mcpgo.Min(1), mcpgo.Max(100), mcpgo.DefaultNumber(10), mcpgo.Description("Collection batch limit; collection also stops after 10 seconds")),
		mcpgo.WithInteger("max_bytes", mcpgo.Min(256), mcpgo.Max(1000000), mcpgo.DefaultNumber(16384), mcpgo.Description("JSON byte limit; complete records retained with omitted count")),
	}
}

func parseTelemetryOptions(req mcpgo.CallToolRequest) (telemetryOptionsValue, error) {
	o := telemetryOptionsValue{format: req.GetString("format", "compact")}
	if v, exists := req.GetArguments()["format"]; exists && v != "compact" && v != "otlp" {
		return o, fmt.Errorf("format must be compact or otlp")
	}
	var err error
	o.maxBytes, err = ros2Int(req, "max_bytes", 16384, 256, 1000000)
	if err != nil {
		return o, err
	}
	o.maxRecords, err = ros2Int(req, "max_records", 100, 1, 1000)
	if err != nil {
		return o, err
	}
	o.maxBatches, err = ros2Int(req, "max_batches", 10, 1, 100)
	return o, err
}

func telemetryResult(kind string, batches []json.RawMessage, opts telemetryOptionsValue) *mcpgo.CallToolResult {
	if opts.format == "otlp" {
		return okListBounded("batches", batches, opts.maxBytes)
	}
	rows, err := compactTelemetryRows(kind, batches)
	if err != nil {
		return errResultf(errCodeInternal, "decoding telemetry: %v", err)
	}
	metadata := map[string]any{"batches_collected": len(batches), "collection_limited": len(batches) >= opts.maxBatches}
	return okRowsBounded(kind, rows, metadata, opts.maxBytes, opts.maxRecords)
}

// Flatten the OTLP transport wrappers while retaining measurement fields,
// resource identity, attributes, timestamps, and instrumentation scope. Raw
// OTLP is still available for consumers that require its original envelope.
func compactTelemetryRows(kind string, batches []json.RawMessage) ([]map[string]any, error) {
	resourcesKey, scopesKey, recordsKey := "resourceLogs", "scopeLogs", "logRecords"
	switch kind {
	case "metrics":
		resourcesKey, scopesKey, recordsKey = "resourceMetrics", "scopeMetrics", "metrics"
	case "traces":
		resourcesKey, scopesKey, recordsKey = "resourceSpans", "scopeSpans", "spans"
	}
	rows := []map[string]any{}
	for _, batch := range batches {
		var root map[string]any
		decoder := json.NewDecoder(bytes.NewReader(batch))
		decoder.UseNumber() // Do not round integers in vendor-supplied fields.
		if err := decoder.Decode(&root); err != nil {
			return nil, err
		}
		for _, resourceValue := range telemetryArray(telemetryObject(root[kind])[resourcesKey]) {
			resource := telemetryObject(resourceValue)
			attrs := telemetryAttributes(telemetryObject(resource["resource"])["attributes"])
			for _, scopeValue := range telemetryArray(resource[scopesKey]) {
				scope := telemetryObject(scopeValue)
				for _, recordValue := range telemetryArray(scope[recordsKey]) {
					record := telemetryObject(recordValue)
					appendRow := func(row map[string]any) {
						if len(attrs) > 0 {
							row["resource"] = attrs
						}
						if instrumentation := telemetryObject(scope["scope"]); len(instrumentation) > 0 {
							row["scope"] = instrumentation
						}
						if attributes, exists := row["attributes"]; exists {
							row["attributes"] = telemetryAttributes(attributes)
						}
						rows = append(rows, row)
					}
					if kind != "metrics" {
						if body, exists := record["body"]; exists {
							record["body"] = telemetryValue(body)
						}
						appendRow(record)
						continue
					}
					// Keep histogram, summary, and temporality semantics. Each row
					// describes one point rather than a resource/scope/metric tree.
					for _, metricKind := range []string{"gauge", "sum", "histogram", "exponentialHistogram", "summary"} {
						data := telemetryObject(record[metricKind])
						for _, pointValue := range telemetryArray(data["dataPoints"]) {
							point := telemetryObject(pointValue)
							point["name"], point["kind"] = record["name"], metricKind
							for _, key := range []string{"unit", "description"} {
								if v, ok := record[key]; ok {
									point[key] = v
								}
							}
							for _, key := range []string{"aggregationTemporality", "isMonotonic"} {
								if v, ok := data[key]; ok {
									point[key] = v
								}
							}
							appendRow(point)
						}
					}
				}
			}
		}
	}
	return rows, nil
}

func telemetryObject(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func telemetryArray(value any) []any {
	a, _ := value.([]any)
	return a
}

func telemetryAttributes(value any) map[string]any {
	attrs := map[string]any{}
	for _, value := range telemetryArray(value) {
		attribute := telemetryObject(value)
		if key, ok := attribute["key"].(string); ok {
			attrs[key] = telemetryValue(attribute["value"])
		}
	}
	return attrs
}

func telemetryValue(value any) any {
	object := telemetryObject(value)
	for _, key := range []string{"stringValue", "boolValue", "intValue", "doubleValue", "bytesValue"} {
		if v, exists := object[key]; exists {
			return v
		}
	}
	if array, exists := object["arrayValue"]; exists {
		values := []any{}
		for _, v := range telemetryArray(telemetryObject(array)["values"]) {
			values = append(values, telemetryValue(v))
		}
		return values
	}
	if kv, exists := object["kvlistValue"]; exists {
		return telemetryAttributes(telemetryObject(kv)["values"])
	}
	return value
}
