import test from "node:test";
import assert from "node:assert/strict";
import { telemetryRows } from "../lib/telemetry.ts";
import { updateMetricHistory, formatMetricValue, metricTitle } from "../lib/metric-charts.ts";
const timestamp = (ms) => String(BigInt(ms) * 1000000n);
function rows({ at = 10000, start = 1000, value = 3, mode = "user", name = "process.cpu.time", unit = "s", resource = [], scope = "agent", gauge = false } = {}) {
  return telemetryRows("metrics", { metrics: { resourceMetrics: [{
    resource: { attributes: [{ key: "service.name", value: { stringValue: "wendy-agent" } }, ...resource] },
    scopeMetrics: [{ scope: { name: scope }, metrics: [{ name, unit, [gauge ? "gauge" : "sum"]: {
      isMonotonic: true, aggregationTemporality: "AGGREGATION_TEMPORALITY_CUMULATIVE",
      dataPoints: [{ asDouble: value, timeUnixNano: timestamp(at), startTimeUnixNano: timestamp(start), attributes: [{ key: "cpu.mode", value: { stringValue: mode } }] }],
    } }] }],
  }] } });
}
test("CPU cumulative seconds become percent of one core and retain separate modes", () => {
  let history = updateMetricHistory([], [...rows(), ...rows({ mode: "system", value: 2 })]);
  assert.equal(history[0].latest, null);
  history = updateMetricHistory(history, [...rows({ at: 11000, value: 5 }), ...rows({ at: 11000, mode: "system", value: 2.25 })]);
  assert.equal(history.find((s) => s.subtitle === "User").latest, 200);
  assert.equal(history.find((s) => s.subtitle === "System").latest, 25);
  assert.equal(history[0].title, "CPU usage");
  assert.equal(history[0].serviceLabel, "Wendy agent");
  assert.equal(history[0].unit, "%");
});
test("reset values or measurement windows break the line instead of creating spikes", () => {
  let history = updateMetricHistory([], rows());
  history = updateMetricHistory(history, rows({ at: 11000, value: 1 }));
  assert.equal(history[0].latest, null);
  history = updateMetricHistory(history, rows({ at: 12000, start: 11500, value: 2 }));
  assert.equal(history[0].latest, null);
  history = updateMetricHistory(history, rows({ at: 13000, start: 11500, value: 2.5 }));
  assert.equal(history[0].latest, 50);
});
test("history sorts backfill, deduplicates timestamps and does not lose earlier points", () => {
  let history = updateMetricHistory([], rows({ at: 12000, value: 4 }));
  history = updateMetricHistory(history, rows({ at: 10000, value: 3 }));
  history = updateMetricHistory(history, rows({ at: 12000, value: 4 }));
  assert.deepEqual(history[0].points, [{ time: 10000, value: null }, { time: 12000, value: 50 }]);
});
test("resource and instrumentation scope identify separate series, attribute order does not", () => {
  const attrs = [{ key: "service.version", value: { stringValue: "v1" } }, { key: "host.name", value: { stringValue: "robot" } }];
  assert.equal(rows({ resource: attrs })[0].key, rows({ resource: attrs.toReversed() })[0].key);
  assert.notEqual(rows({ scope: "a" })[0].key, rows({ scope: "b" })[0].key);
  assert.notEqual(rows({ resource: attrs })[0].key, rows()[0].key);
});
test("gauge, custom and histogram metrics preserve their meaning", () => {
  let history = updateMetricHistory([], rows({ name: "process.memory.usage", unit: "By", value: 132636672, gauge: true }));
  assert.equal(history[0].latest, 132636672);
  assert.equal(history[0].title, "Memory usage");
  assert.equal(formatMetricValue(history[0].latest, history[0].unit), "132.64 MB");
  assert.equal(metricTitle("robot.custom.thing"), "robot.custom.thing");
  const distribution = telemetryRows("metrics", { metrics: { resourceMetrics: [{ scopeMetrics: [{ metrics: [{ name: "http.server.request.duration", unit: "s", histogram: { dataPoints: [{ timeUnixNano: timestamp(10000), count: "4", sum: 2 }] } }] }] }] } });
  history = updateMetricHistory([], distribution);
  assert.equal(history[0].kind, "distribution");
  assert.equal(history[0].title, "Request duration · average");
  assert.equal(history[0].latest, 0.5);
  assert.equal(formatMetricValue(0.5, "s"), "500 ms");
});
test("history remains bounded and missing/nonfinite data never creates invented values", () => {
  let history = [];
  for (let i = 1; i <= 140; i++) history = updateMetricHistory(history, rows({ at: i * 1000, value: i }));
  assert.equal(history[0].points.length, 120);
  for (let i = 0; i < 130; i++) history = updateMetricHistory(history, rows({ name: `custom-${i}`, at: 200000, value: i }));
  assert.equal(history.length, 100);
  assert.equal(updateMetricHistory([], rows({ at: 0 })).length, 0);
  assert.equal(updateMetricHistory([], rows({ value: Infinity }))[0].latest, null);
  assert.equal(formatMetricValue(1000000000, "By"), "1 GB");
  assert.equal(formatMetricValue(0, "By"), "0 B");
});
