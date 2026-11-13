package backend

import (
	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	"strings"
)

type LogLevel uint32

const (
	LogUnknown LogLevel = iota
	LogTrace
	LogDebug
	LogInfo
	LogWarn
	LogError
	LogFatal
)

type LogRecord struct {
	Timestamp          uint64
	Level              LogLevel
	Body, App, Service string
}

func desktopLog(record *logs.LogRecord, attributes []*common.KeyValue) *LogRecord {
	level := LogUnknown
	if n := int32(record.GetSeverityNumber()); n >= 1 && n <= 24 {
		level = LogLevel((n-1)/4 + 1)
	} else {
		switch strings.ToUpper(record.GetSeverityText()) {
		case "TRACE":
			level = LogTrace
		case "DEBUG":
			level = LogDebug
		case "INFO":
			level = LogInfo
		case "WARN", "WARNING":
			level = LogWarn
		case "ERROR":
			level = LogError
		case "FATAL", "CRITICAL":
			level = LogFatal
		}
	}
	body := record.GetBody().GetStringValue()
	if body == "" && record.GetBody() != nil {
		body = record.GetBody().String()
	}
	result := &LogRecord{Timestamp: record.GetTimeUnixNano(), Level: level, Body: body}
	if result.Timestamp == 0 {
		result.Timestamp = record.GetObservedTimeUnixNano()
	}
	for _, set := range [][]*common.KeyValue{attributes, record.GetAttributes()} {
		for _, a := range set {
			switch a.GetKey() {
			case "app_name", "wendy.app.name", "service.namespace":
				result.App = a.GetValue().GetStringValue()
			case "service_name", "service.name":
				result.Service = a.GetValue().GetStringValue()
			}
		}
	}
	return result
}
