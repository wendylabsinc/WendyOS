import assert from "node:assert/strict";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { build } from "esbuild";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

const compiled = await build({
  entryPoints: [
    fileURLToPath(new URL("../src/telemetry.tsx", import.meta.url)),
  ],
  bundle: true,
  write: false,
  format: "esm",
  platform: "node",
  plugins: [
    {
      name: "react-imports",
      setup(builder) {
        builder.onResolve({ filter: /^react(?:\/|$)/ }, ({ path }) => ({
          path: import.meta.resolve(path),
          external: true,
        }));
      },
    },
  ],
});
const { metricSeries, telemetryTime, metricValue, TelemetryPanel } =
  await import(
    "data:text/javascript;base64," +
      Buffer.from(compiled.outputFiles[0].text).toString("base64")
  );

function point(second, value, extra = {}) {
  return {
    name: "container.memory.usage",
    kind: "gauge",
    unit: "By",
    timeUnixNano: String(
      1_800_000_000_000_000_000n + BigInt(second) * 1_000_000_000n,
    ),
    asInt: String(value),
    resource: { "service.name": "badge-reader" },
    ...extra,
  };
}
function render(kind, rows, extra = {}) {
  return renderToStaticMarkup(
    createElement(TelemetryPanel, {
      kind,
      data: { [kind.toLowerCase()]: rows, ...extra },
      loading: false,
    }),
  );
}

test("history preserves series identity, orders time, and removes replay duplicates", () => {
  const input = [
    point(2, 4096),
    point(0, 1024),
    point(1, 2048),
    point(2, 4096),
  ];
  const result = metricSeries([
    ...input,
    point(1, 20, { resource: { "service.name": "other-app" } }),
    point(1, 40, { attributes: { "cpu.mode": "system" } }),
    point(1, 60, { unit: "s" }),
    point(1, 80, { scope: { name: "different-instrument" } }),
  ]);
  assert.equal(
    result.length,
    5,
    "unrelated measurements must not form a trend",
  );
  assert.deepEqual(
    result[0].points.map((p) => p.value),
    [1024, 2048, 4096],
  );
  assert.equal(result[0].latestValue, "4096");
  assert.equal(input[0].asInt, "4096", "grouping must not mutate tool results");
  const equivalent = metricSeries([
    point(0, 1, { attributes: { a: "one", b: "two" } }),
    point(1, 2, { attributes: { b: "two", a: "one" } }),
  ]);
  assert.equal(equivalent.length, 1);
  assert.equal(equivalent[0].points.length, 2);
});

test("only timestamped finite values become chart points", () => {
  const rows = [
    point(0, 0),
    point(1, 2, { timeUnixNano: undefined }),
    point(2, 3, { timeUnixNano: "invalid" }),
    point(3, "NaN"),
    point(4, "9223372036854775807"),
    point(5, 0, { flags: 1 }),
    point(6, 4),
  ];
  assert.deepEqual(
    metricSeries(rows)[0].points.map((p) => p.value),
    [0, 4],
  );
  assert.equal(
    telemetryTime({ timeUnixNano: "1800000000123456789" }),
    1800000000123,
  );
  assert.equal(
    telemetryTime({ observed_at: "2026-09-30T20:00:00Z" }),
    Date.parse("2026-09-30T20:00:00Z"),
  );
  assert.equal(telemetryTime({ timeUnixNano: "0" }), undefined);
});

test("distribution totals retain units and aggregation semantics", () => {
  const [sum] = metricSeries([
    point(1, 0, {
      kind: "histogram",
      name: "latency",
      unit: "ms",
      sum: 200,
      count: "4",
      aggregationTemporality: "AGGREGATION_TEMPORALITY_DELTA",
    }),
  ]);
  assert.equal(sum.latestValue, 200);
  assert.equal(sum.unit, "ms");
  assert.match(sum.meaning, /Sum of observations.*Per interval/);
  const [count] = metricSeries([
    point(1, 0, { kind: "histogram", count: "4" }),
  ]);
  assert.equal(count.latestValue, "4");
  assert.equal(count.unit, "observations");
  assert.match(metricValue(1024 ** 2 * 1.5, "By"), /^1[.,]5 MiB$/);
  assert.equal(metricValue("9223372036854775807", "1"), "9223372036854775807");
});

test("single samples do not invent a line; real history renders one accessible chart", () => {
  const single = render("Metrics", [point(0, 1024)]);
  assert.match(single, /One timestamped sample/);
  assert.doesNotMatch(single, /class="metric-line"/);
  const chart = render("Metrics", [
    point(2, 3072),
    point(0, 1024),
    point(1, 2048),
  ]);
  assert.equal((chart.match(/class="metric-card"/g) || []).length, 1);
  assert.equal((chart.match(/class="metric-line"/g) || []).length, 1);
  assert.match(chart, /role="img"/);
  assert.match(chart, /3 recorded samples/);
  assert.doesNotMatch(chart, /NaN|Infinity/);
  const constant = render("Metrics", [point(0, 0), point(1, 0)]);
  assert.doesNotMatch(constant, /NaN|Infinity/);
});

test("logs keep time, level, and multiline message in separate columns", () => {
  const markup = render(
    "Logs",
    [
      {
        timeUnixNano: "1800000000000000000",
        severityNumber: 17,
        body: "First line\n  <script>untrusted log text</script>",
      },
    ],
    { omitted: 9 },
  );
  assert.match(markup, /<time dateTime=/);
  assert.match(
    markup,
    /<span class="log-level">ERROR<\/span><span class="log-message">/,
  );
  assert.match(markup, /First line\n  &lt;script&gt;/);
  assert.match(markup, /9 additional records omitted/);
});
