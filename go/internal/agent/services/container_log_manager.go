package services

import (
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	otelpb "github.com/wendylabsinc/wendy/proto/gen/otelpb"
)

// logSubscriber wraps a delivery channel with a mutex that guards the closed
// state so that Publish and Unsubscribe cannot race on close vs send.
type logSubscriber struct {
	mu     sync.Mutex
	ch     chan ContainerOutput
	closed bool
}

// send attempts a non-blocking send to the subscriber.
// Returns false if the subscriber has already been closed.
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

// ContainerLogManager manages multi-subscriber fan-out for container output
// and bridges container stdout/stderr to the TelemetryBroadcaster as OTEL log records.
type ContainerLogManager struct {
	logger      *zap.Logger
	broadcaster *TelemetryBroadcaster
	mu          sync.Mutex
	subscribers map[string]map[string]*logSubscriber // appName -> subID -> subscriber
	nextID      uint64
	resources   map[string]*otelpb.Resource // appName -> pre-built OTel resource (protected by mu)
}

// NewContainerLogManager creates a new ContainerLogManager.
func NewContainerLogManager(logger *zap.Logger, broadcaster *TelemetryBroadcaster) *ContainerLogManager {
	return &ContainerLogManager{
		logger:      logger,
		broadcaster: broadcaster,
		subscribers: make(map[string]map[string]*logSubscriber),
		resources:   make(map[string]*otelpb.Resource),
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

// Subscribe creates a new subscription for a container's output.
// Returns the subscription ID and a read-only channel of ContainerOutput.
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

// Unsubscribe removes a subscription and closes its channel.
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

// Publish sends output to all subscribers for a container and to the telemetry broadcaster.
func (m *ContainerLogManager) Publish(appName string, output ContainerOutput) {
	// Bridge to OTEL telemetry broadcaster.
	m.publishToTelemetry(appName, output)

	// Fan out to all subscribers.
	m.mu.Lock()
	appSubs := m.subscribers[appName]
	// Copy subscriber pointers under lock to avoid holding it while sending.
	subs := make([]*logSubscriber, 0, len(appSubs))
	for _, sub := range appSubs {
		subs = append(subs, sub)
	}
	m.mu.Unlock()

	for _, sub := range subs {
		sub.send(output)
	}
}

// publishToTelemetry converts container output into OTEL log records and
// publishes them via the TelemetryBroadcaster.
func (m *ContainerLogManager) publishToTelemetry(appName string, output ContainerOutput) {
	if output.Done {
		return
	}

	now := uint64(time.Now().UnixNano())
	var records []*otelpb.LogRecord

	if len(output.Stdout) > 0 {
		records = append(records, &otelpb.LogRecord{
			TimeUnixNano:         now,
			ObservedTimeUnixNano: now,
			SeverityNumber:       otelpb.SeverityNumber_SEVERITY_NUMBER_INFO,
			SeverityText:         "INFO",
			Body: &otelpb.AnyValue{
				Value: &otelpb.AnyValue_StringValue{
					StringValue: string(output.Stdout),
				},
			},
			Attributes: []*otelpb.KeyValue{
				{
					Key: "stream",
					Value: &otelpb.AnyValue{
						Value: &otelpb.AnyValue_StringValue{StringValue: "stdout"},
					},
				},
			},
		})
	}

	if len(output.Stderr) > 0 {
		records = append(records, &otelpb.LogRecord{
			TimeUnixNano:         now,
			ObservedTimeUnixNano: now,
			SeverityNumber:       otelpb.SeverityNumber_SEVERITY_NUMBER_WARN,
			SeverityText:         "WARN",
			Body: &otelpb.AnyValue{
				Value: &otelpb.AnyValue_StringValue{
					StringValue: string(output.Stderr),
				},
			},
			Attributes: []*otelpb.KeyValue{
				{
					Key: "stream",
					Value: &otelpb.AnyValue{
						Value: &otelpb.AnyValue_StringValue{StringValue: "stderr"},
					},
				},
			},
		})
	}

	if len(records) == 0 {
		return
	}

	m.mu.Lock()
	resource := m.resources[appName]
	m.mu.Unlock()
	if resource == nil {
		resource = containerResource(appName, "")
	}

	m.broadcaster.PublishLogs(&otelpb.ExportLogsServiceRequest{
		ResourceLogs: []*otelpb.ResourceLogs{
			{
				Resource: resource,
				ScopeLogs: []*otelpb.ScopeLogs{
					{
						Scope:      &otelpb.InstrumentationScope{Name: "wendy.container"},
						LogRecords: records,
					},
				},
			},
		},
	})
}
