import assert from "node:assert/strict";
import { usage, telemetryRows } from "../lib/telemetry.ts";
const sample = (total, idle, cpu) => ({
  version: {},
  apps: [],
  stats: {
    host: { cpuCount: 4, cpuTotalJiffies: total, cpuIdleJiffies: idle },
    containers: [
      { appName: "camera", cpuUsageNanos: cpu, memoryBytes: "4096" },
    ],
  },
});
const previous = {
  at: 1000,
  sample: sample("90071992547409930", "40000000000000000", "90071992547409930"),
};
const current = {
  at: 2000,
  sample: sample("90071992547410030", "40000000000000025", "90071993547409930"),
};
assert.equal(usage(previous, current).cpu, 75);
assert.equal(usage(previous, current).containers.get("camera").cpu, 25);
assert.equal(usage(null, current).cpu, null);
assert.equal(
  usage(current, { at: 3000, sample: sample("1", "0", "1") }).containers.get(
    "camera",
  ).cpu,
  null,
);
const resource = {
  attributes: [{ key: "service.name", value: { stringValue: "camera" } }],
};
const point = {
  asInt: "0",
  attributes: [{ key: "feed", value: { stringValue: "front" } }],
};
const metric = { name: "frames", unit: "1", sum: { dataPoints: [point] } };
const metrics = telemetryRows("metrics", {
  metrics: {
    resourceMetrics: [{ resource, scopeMetrics: [{ metrics: [metric] }] }],
  },
});
assert.equal(metrics[0].service, "camera");
assert.equal(metrics[0].value, "0 1");
assert.match(metrics[0].key, /feed=front/);
const spans = telemetryRows("traces", {
  traces: {
    resourceSpans: [
      {
        resource,
        scopeSpans: [
          {
            spans: [
              {
                name: "infer",
                traceId: "AQID",
                spanId: "BA==",
                startTimeUnixNano: "900719925474099300",
                endTimeUnixNano: "900719925475099300",
              },
            ],
          },
        ],
      },
    ],
  },
});
assert.equal(spans[0].duration, 1);
assert.equal(spans[0].trace, "010203");
assert.equal(
  telemetryRows("logs", {
    logs: {
      resourceLogs: [
        {
          resource,
          scopeLogs: [
            {
              logRecords: [
                { severityText: "INFO", body: { stringValue: "ready" } },
              ],
            },
          ],
        },
      ],
    },
  })[0].value,
  "ready",
);
console.log(
  "PASS: precise CPU deltas, restart handling, OTLP metric labels/zero values, trace duration, logs",
);
const severityCases = [
  [{ severityText: "DEBUG" }, "debug"],
  [{ severityText: "WARNING" }, "warn"],
  [{ severityText: "critical" }, "fatal"],
  [{ severityNumber: "SEVERITY_NUMBER_INFO3" }, "info"],
  [{ severityNumber: "SEVERITY_NUMBER_UNSPECIFIED", severityText: "ERROR" }, "error"],
  [{ severityNumber: 0 }, "unknown"],
  [{ severityNumber: 25 }, "unknown"],
  ...Array.from({ length: 24 }, (_, i) => [
    { severityNumber: i + 1 },
    ["trace", "debug", "info", "warn", "error", "fatal"][Math.floor(i / 4)],
  ]),
];
for (const [record, expected] of severityCases) {
  const [row] = telemetryRows("logs", {
    logs: { resourceLogs: [{ scopeLogs: [{ logRecords: [record] }] }] },
  });
  assert.equal(row.severity, expected, JSON.stringify(record));
  assert.equal(row.name, record.severityText || (expected === "unknown" ? "LOG" : expected.toUpperCase()));
}
console.log("PASS: OTLP numeric and named severity levels, text aliases, unknown levels");
