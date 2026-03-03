package services

import (
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	otelpb "github.com/wendylabsinc/wendy/proto/gen/otelpb"
)

// ContainerLogManager manages multi-subscriber fan-out for container output
// and bridges container stdout/stderr to the TelemetryBroadcaster as OTEL log records.
type ContainerLogManager struct {
	logger      *zap.Logger
	broadcaster *TelemetryBroadcaster
	mu          sync.Mutex
	subscribers map[string]map[string]chan ContainerOutput // appName -> subID -> channel
	nextID      uint64
}

// NewContainerLogManager creates a new ContainerLogManager.
func NewContainerLogManager(logger *zap.Logger, broadcaster *TelemetryBroadcaster) *ContainerLogManager {
	return &ContainerLogManager{
		logger:      logger,
		broadcaster: broadcaster,
		subscribers: make(map[string]map[string]chan ContainerOutput),
	}
}

// Subscribe creates a new subscription for a container's output.
// Returns the subscription ID and a read-only channel of ContainerOutput.
func (m *ContainerLogManager) Subscribe(appName string) (string, <-chan ContainerOutput) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.nextID++
	subID := fmt.Sprintf("log-sub-%d", m.nextID)

	if m.subscribers[appName] == nil {
		m.subscribers[appName] = make(map[string]chan ContainerOutput)
	}

	ch := make(chan ContainerOutput, 64)
	m.subscribers[appName][subID] = ch

	m.logger.Debug("Container log subscriber added",
		zap.String("app_name", appName),
		zap.String("sub_id", subID),
	)

	return subID, ch
}

// Unsubscribe removes a subscription and closes its channel.
func (m *ContainerLogManager) Unsubscribe(appName string, subID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	appSubs, ok := m.subscribers[appName]
	if !ok {
		return
	}

	if ch, exists := appSubs[subID]; exists {
		close(ch)
		delete(appSubs, subID)
	}

	if len(appSubs) == 0 {
		delete(m.subscribers, appName)
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
	// Copy the map values under lock to avoid holding it while sending.
	channels := make([]chan ContainerOutput, 0, len(appSubs))
	for _, ch := range appSubs {
		channels = append(channels, ch)
	}
	m.mu.Unlock()

	for _, ch := range channels {
		select {
		case ch <- output:
		default:
			// Drop if subscriber is slow.
		}
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

	m.broadcaster.PublishLogs(&otelpb.ExportLogsServiceRequest{
		ResourceLogs: []*otelpb.ResourceLogs{
			{
				Resource: &otelpb.Resource{
					Attributes: []*otelpb.KeyValue{
						{
							Key: "service.name",
							Value: &otelpb.AnyValue{
								Value: &otelpb.AnyValue_StringValue{StringValue: appName},
							},
						},
					},
				},
				ScopeLogs: []*otelpb.ScopeLogs{
					{
						Scope: &otelpb.InstrumentationScope{
							Name: "wendy.container",
						},
						LogRecords: records,
					},
				},
			},
		},
	})
}
