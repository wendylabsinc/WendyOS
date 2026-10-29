package services

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

type logSubscriber struct {
	mu     sync.Mutex
	ch     chan ContainerOutput
	closed bool
}

// send attempts a non-blocking send to the subscriber.
// It is a no-op if the subscriber is already closed; the mutex ensures this
// check and the channel send cannot race with close.
func (s *logSubscriber) send(output ContainerOutput) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- output:
	default:
		// Drop if subscriber is slow.
	}
}

// close marks the subscriber as closed and closes the underlying channel.
// Safe to call once; subsequent calls are no-ops.
func (s *logSubscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}

type ContainerLogManager struct {
	logger      *zap.Logger
	broadcaster TelemetryPublisher
	mu          sync.RWMutex
	subscribers map[string]map[string]*logSubscriber // appName -> subID -> subscriber
	nextID      uint64
	resources   map[string]*resourcepb.Resource // appName -> pre-built OTel resource (protected by mu)
}

// NewContainerLogManager creates a new ContainerLogManager.
func NewContainerLogManager(logger *zap.Logger, broadcaster TelemetryPublisher) *ContainerLogManager {
	return &ContainerLogManager{
		logger:      logger,
		broadcaster: broadcaster,
		subscribers: make(map[string]map[string]*logSubscriber),
		resources:   make(map[string]*resourcepb.Resource),
	}
}

// RegisterApp caches the OTel resource for an app so that its stdout/stderr logs
// carry service.namespace, service.version, and service.instance.id.
func (m *ContainerLogManager) RegisterApp(appName, version string) {
	resource := containerResource(appName, version)
	m.mu.Lock()
	m.resources[appName] = resource
	m.mu.Unlock()
}

func (m *ContainerLogManager) Subscribe(appName string) (string, <-chan ContainerOutput) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.nextID++
	subID := fmt.Sprintf("log-sub-%d", m.nextID)

	if m.subscribers[appName] == nil {
		m.subscribers[appName] = make(map[string]*logSubscriber)
	}

	sub := &logSubscriber{ch: make(chan ContainerOutput, 64)}
	m.subscribers[appName][subID] = sub

	m.logger.Debug("Container log subscriber added",
		zap.String("app_name", appName),
		zap.String("sub_id", subID),
	)

	return subID, sub.ch
}

func (m *ContainerLogManager) Unsubscribe(appName string, subID string) {
	m.mu.Lock()

	appSubs, ok := m.subscribers[appName]
	if !ok {
		m.mu.Unlock()
		return
	}

	sub, exists := appSubs[subID]
	if exists {
		delete(appSubs, subID)
	}
	if len(appSubs) == 0 {
		delete(m.subscribers, appName)
	}

	m.mu.Unlock()

	// Close outside the manager lock so that an in-flight Publish sending to
	// this subscriber's channel can acquire sub.mu without deadlocking.
	if exists {
		sub.close()
	}

	m.logger.Debug("Container log subscriber removed",
		zap.String("app_name", appName),
		zap.String("sub_id", subID),
	)
}

func (m *ContainerLogManager) Publish(appName string, output ContainerOutput) {
	if output.PublishIfCurrent != nil {
		guard := output.PublishIfCurrent
		output.PublishIfCurrent = nil
		guard(func() { m.Publish(appName, output) })
		return
	}
	m.publishToTelemetry(appName, output)

	// Fan out to all subscribers.
	m.mu.RLock()
	appSubs := m.subscribers[appName]
	for _, sub := range appSubs {
		sub.send(output)
	}
	m.mu.RUnlock()
}

// publishToTelemetry converts container output into OTEL log records and
// publishes them via the TelemetryBroadcaster.
func (m *ContainerLogManager) publishToTelemetry(appName string, output ContainerOutput) {
	if output.Done {
		return
	}

	now := uint64(time.Now().UnixNano())
	var records []*logspb.LogRecord

	if len(output.Stdout) > 0 {
		records = append(records, containerLogRecord(now, "stdout", output.Stdout))
	}

	if len(output.Stderr) > 0 {
		records = append(records, containerLogRecord(now, "stderr", output.Stderr))
	}

	if len(records) == 0 {
		return
	}

	m.mu.RLock()
	resource := m.resources[appName]
	m.mu.RUnlock()
	if resource == nil {
		resource = containerResource(appName, "")
	}

	m.broadcaster.PublishLogs(&collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{
			{
				Resource: resource,
				ScopeLogs: []*logspb.ScopeLogs{
					{
						Scope:      &commonpb.InstrumentationScope{Name: "wendy.container"},
						LogRecords: records,
					},
				},
			},
		},
	})
}

func containerLogRecord(now uint64, stream string, body []byte) *logspb.LogRecord {
	severityNumber, severityText := inferContainerLogSeverity(body)
	return &logspb.LogRecord{
		TimeUnixNano:         now,
		ObservedTimeUnixNano: now,
		SeverityNumber:       severityNumber,
		SeverityText:         severityText,
		Body: &commonpb.AnyValue{
			Value: &commonpb.AnyValue_StringValue{
				StringValue: string(body),
			},
		},
		Attributes: []*commonpb.KeyValue{
			{
				Key: "stream",
				Value: &commonpb.AnyValue{
					Value: &commonpb.AnyValue_StringValue{StringValue: stream},
				},
			},
		},
	}
}

func inferContainerLogSeverity(body []byte) (logspb.SeverityNumber, string) {
	line := strings.TrimSpace(string(body))
	if line == "" {
		return logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "INFO"
	}

	if severity, text, ok := severityFromJSONLine(line); ok {
		return severity, text
	}
	if severity, text, ok := severityFromLeadingLevel(line); ok {
		return severity, text
	}
	if severity, text, ok := severityFromISOLevel(line); ok {
		return severity, text
	}
	if severity, text, ok := severityFromLlamaPrefix(line); ok {
		return severity, text
	}

	// stderr is not inherently a warning stream: many runtimes put routine
	// diagnostics there. Keep the raw stream as an attribute and use INFO as the
	// neutral default so warning/error filters only surface content that actually
	// advertises a warning or error level.
	return logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "INFO"
}

func severityFromJSONLine(line string) (logspb.SeverityNumber, string, bool) {
	if !strings.HasPrefix(line, "{") {
		return 0, "", false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return 0, "", false
	}
	for _, key := range []string{"severity", "level"} {
		value, ok := obj[key]
		if !ok {
			continue
		}
		if s, ok := value.(string); ok {
			if severity, text, ok := containerLogSeverityFromToken(s); ok {
				return severity, text, true
			}
		}
	}
	return 0, "", false
}

func severityFromLeadingLevel(line string) (logspb.SeverityNumber, string, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0, "", false
	}
	return containerLogSeverityFromToken(fields[0])
}

func severityFromISOLevel(line string) (logspb.SeverityNumber, string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || !looksLikeISOTimestamp(fields[0]) {
		return 0, "", false
	}
	for _, field := range fields[1:min(len(fields), 4)] {
		if severity, text, ok := containerLogSeverityFromToken(field); ok {
			return severity, text, true
		}
	}
	return 0, "", false
}

func severityFromLlamaPrefix(line string) (logspb.SeverityNumber, string, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0, "", false
	}
	if severity, text, ok := containerLogSeverityFromLlamaToken(fields[0]); ok {
		return severity, text, true
	}
	if len(fields) >= 2 && looksLikeTimestampPrefix(fields[0]) {
		return containerLogSeverityFromLlamaToken(fields[1])
	}
	return 0, "", false
}

func looksLikeISOTimestamp(s string) bool {
	return len(s) >= len("2006-01-02T") && s[4] == '-' && s[7] == '-' && (s[10] == 'T' || s[10] == 't')
}

func looksLikeTimestampPrefix(s string) bool {
	hasDigit := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '.' || r == ':' || r == '-' || r == 'T' || r == 't' || r == 'Z' || r == 'z' || r == '+':
		default:
			return false
		}
	}
	return hasDigit
}

func containerLogSeverityFromToken(token string) (logspb.SeverityNumber, string, bool) {
	token = strings.TrimSpace(token)
	if idx := strings.IndexAny(token, "="); idx >= 0 && idx+1 < len(token) {
		token = token[idx+1:]
	}
	token = strings.ToLower(strings.Trim(token, "[](){}:;,|\"'"))
	return containerLogSeverityFromLevel(token)
}

func containerLogSeverityFromLlamaToken(token string) (logspb.SeverityNumber, string, bool) {
	switch strings.Trim(token, "[](){}:;,|\"'") {
	case "I":
		return logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "INFO", true
	case "W":
		return logspb.SeverityNumber_SEVERITY_NUMBER_WARN, "WARN", true
	case "E":
		return logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "ERROR", true
	case "D":
		return logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG, "DEBUG", true
	default:
		return 0, "", false
	}
}

func containerLogSeverityFromLevel(level string) (logspb.SeverityNumber, string, bool) {
	switch level {
	case "trace":
		return logspb.SeverityNumber_SEVERITY_NUMBER_TRACE, "TRACE", true
	case "debug", "dbg":
		return logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG, "DEBUG", true
	case "info", "information", "notice":
		return logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "INFO", true
	case "warn", "warning", "wrn":
		return logspb.SeverityNumber_SEVERITY_NUMBER_WARN, "WARN", true
	case "error", "err":
		return logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "ERROR", true
	case "fatal", "panic", "critical", "crit":
		return logspb.SeverityNumber_SEVERITY_NUMBER_FATAL, "FATAL", true
	default:
		return 0, "", false
	}
}
