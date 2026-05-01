package services

import (
	"context"
	"time"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	otelpb "github.com/wendylabsinc/wendy/proto/gen/otelpb"
)

const metricsCollectionInterval = 15 * time.Second

// CollectContainerMetrics periodically samples CPU and memory for all running
// Wendy-managed containers and publishes OTel metrics. When logManager is non-nil,
// it registers each container so that stdout/stderr logs carry full resource attrs.
func CollectContainerMetrics(
	ctx context.Context,
	client ContainerdClient,
	broadcaster *TelemetryBroadcaster,
	logManager *ContainerLogManager,
) {
	// cache per app so we don't rebuild on every tick
	resources := make(map[string]*otelpb.Resource)
	startTimes := make(map[string]time.Time)

	collect := func(t time.Time) {
		containers, err := client.ListContainers(ctx)
		if err != nil {
			return
		}
		active := make(map[string]bool, len(containers))
		for _, c := range containers {
			appName := c.GetAppName()
			active[appName] = true

			if _, seen := startTimes[appName]; !seen {
				startTimes[appName] = t
				version := c.GetAppVersion()
				resources[appName] = containerResource(appName, version)
				if logManager != nil {
					logManager.RegisterApp(appName, version)
				}
			}

			if c.GetRunningState() != agentpb.AppRunningState_RUNNING {
				continue
			}
			m, err := client.GetContainerMetrics(ctx, appName)
			if err != nil {
				continue
			}
			publishProcessMetrics(broadcaster, resources[appName], "wendy.container",
				"container.cpu.usage", "container.memory.usage",
				m.UserCPUNanos, m.SysCPUNanos, m.MemBytes, startTimes[appName], t)
		}
		// Evict caches for containers that no longer exist.
		for name := range startTimes {
			if !active[name] {
				delete(startTimes, name)
				delete(resources, name)
			}
		}
	}

	collect(time.Now())
	ticker := time.NewTicker(metricsCollectionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			collect(t)
		}
	}
}

// containerResource builds the standard OTel resource for a Wendy container app.
func containerResource(appName, version string) *otelpb.Resource {
	attrs := []*otelpb.KeyValue{
		stringKV("service.name", appName),
		stringKV("service.namespace", "wendy"),
	}
	if version != "" {
		attrs = append(attrs, stringKV("service.version", version))
	}
	if h := resolveHostname(); h != "" {
		attrs = append(attrs, stringKV("service.instance.id", h))
	}
	return &otelpb.Resource{Attributes: attrs}
}

// publishProcessMetrics emits one OTel metrics export with cpu.time (user+system)
// and memory.usage for the given resource and instrumentation scope.
// cpuMetric must be a Sum/monotonic metric name; memMetric a Gauge.
// startTime is the start of the cumulative measurement window (required by OTel for Sum metrics).
func publishProcessMetrics(
	broadcaster *TelemetryBroadcaster,
	resource *otelpb.Resource,
	scope, cpuMetric, memMetric string,
	userCPUNanos, sysCPUNanos, memBytes int64,
	startTime, t time.Time,
) {
	nowNano := uint64(t.UnixNano())
	startNano := uint64(startTime.UnixNano())
	broadcaster.PublishMetrics(&otelpb.ExportMetricsServiceRequest{
		ResourceMetrics: []*otelpb.ResourceMetrics{
			{
				Resource: resource,
				ScopeMetrics: []*otelpb.ScopeMetrics{
					{
						Scope: &otelpb.InstrumentationScope{Name: scope},
						Metrics: []*otelpb.Metric{
							{
								Name: cpuMetric,
								Unit: "s",
								Data: &otelpb.Metric_Sum{
									Sum: &otelpb.Sum{
										IsMonotonic:            true,
										AggregationTemporality: otelpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
										DataPoints: []*otelpb.NumberDataPoint{
											{
												Attributes:        []*otelpb.KeyValue{stringKV("cpu.mode", "user")},
												StartTimeUnixNano: startNano,
												TimeUnixNano:      nowNano,
												Value:             &otelpb.NumberDataPoint_AsDouble{AsDouble: float64(userCPUNanos) / 1e9},
											},
											{
												Attributes:        []*otelpb.KeyValue{stringKV("cpu.mode", "system")},
												StartTimeUnixNano: startNano,
												TimeUnixNano:      nowNano,
												Value:             &otelpb.NumberDataPoint_AsDouble{AsDouble: float64(sysCPUNanos) / 1e9},
											},
										},
									},
								},
							},
							{
								Name: memMetric,
								Unit: "By",
								Data: &otelpb.Metric_Gauge{
									Gauge: &otelpb.Gauge{
										DataPoints: []*otelpb.NumberDataPoint{
											{
												TimeUnixNano: nowNano,
												Value:        &otelpb.NumberDataPoint_AsInt{AsInt: memBytes},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	})
}
