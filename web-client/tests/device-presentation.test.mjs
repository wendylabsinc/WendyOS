import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";
import ts from "typescript";
import { renderToStaticMarkup } from "react-dom/server";
import { createElement } from "react";
import { formatBytes } from "../lib/client.ts";
const require = createRequire(import.meta.url);
function compile(name) {
  const source = readFileSync(new URL(`../device/${name}`, import.meta.url), "utf8");
  let code = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, jsx: ts.JsxEmit.ReactJSX, target: ts.ScriptTarget.ES2022 } }).outputText;
  code = code.replaceAll('"react/jsx-runtime"', JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href));
  code = code.replaceAll('"react"', JSON.stringify(pathToFileURL(require.resolve("react")).href));
  for (const dependency of ["rpc", "presentation"]) if (code.includes(`"./${dependency}"`)) code = code.replaceAll(`"./${dependency}"`, JSON.stringify(compile(`${dependency}.ts`)));
  return `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`;
}
const { visibleEntries, formatted, unavailableFeature, interfaceKind } = await import(compile("presentation.ts"));
const { default: Result } = await import(compile("Result.tsx"));
test("empty nested data and secrets disappear without losing false or zero", () => {
  const value = { properties: {}, children: [{ properties: {} }], tags: [], password: "secret", active: false, count: 0 };
  assert.deepEqual(visibleEntries(value), [["active", false], ["count", 0]]);
  const html = renderToStaticMarkup(createElement(Result, { value }));
  assert.equal((html.match(/<dt>/g) || []).length, 2);
  assert.doesNotMatch(html, /Properties|Children|Tags|secret|<hr|No additional/);
  assert.match(html, />No</);
  assert.match(html, />0</);
});
test("hardware arrays render aligned table rows instead of nested label dumps", () => {
  const html = renderToStaticMarkup(createElement(Result, { value: { devices: [{ name: "eth0", properties: {} }, { name: "eth1", properties: {} }] } }));
  assert.match(html, /<table/);
  assert.equal((html.match(/<tbody><tr>|<\/tr><tr>/g) || []).length, 2);
  assert.doesNotMatch(html, /Properties|Details<\/summary>/);
});
test("storage and time use readable units", () => {
  assert.equal(formatted("1000000000", "totalBytes"), "1 GB");
  assert.equal(formatted("1073741824", "totalBytes"), "1.1 GB");
  assert.equal(formatted("1000", "usedBytes"), "1 KB");
  assert.equal(formatted("0", "usedBytes"), "0 B");
  assert.equal(formatBytes(1073741824), "1.1 GB");
  assert.equal(formatBytes(1000000), "1 MB");
  assert.equal(formatBytes(null), "—");
  assert.equal(formatted("90061", "uptimeSeconds"), "1d 1h");
  assert.doesNotMatch(formatted("1795005627966701087", "unixNanos"), /1795005627966701087/);
});
test("only explicit unsupported hardware responses hide controls", () => {
  assert.equal(unavailableFeature("rpc error: code = Unavailable desc = power modes are only supported on NVIDIA Jetson devices", "power"), true);
  assert.equal(unavailableFeature("Error: no WiFi device found", "wifi"), true);
  assert.equal(unavailableFeature("connection unavailable", "wifi"), false);
  assert.equal(unavailableFeature("permission denied", "power"), false);
  assert.equal(interfaceKind("eth0"), "Ethernet");
  assert.equal(interfaceKind("enp3s0"), "Ethernet");
  assert.equal(interfaceKind("wg0"), "Tunnel");
});
