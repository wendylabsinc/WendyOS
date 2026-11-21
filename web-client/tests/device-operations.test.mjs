import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import ts from "typescript";
const root = new URL("../../", import.meta.url);
const source = readFileSync(new URL("../device/operations.ts", import.meta.url), "utf8");
const code = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext } }).outputText;
const { sections } = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);
const protocols = new Map();
for (const directory of ["go/proto/gen/agentpb", "go/proto/gen/agentpb/v2"]) {
  const dir = new URL(directory + "/", root);
  for (const name of readdirSync(dir).filter((name) => name.endsWith("_grpc.pb.go"))) {
    const text = readFileSync(new URL(name, dir), "utf8");
    for (const match of text.matchAll(/\w+_FullMethodName\s+= "(\/[^\"]+)"/g)) {
      const method = match[1].split("/").at(-1);
      const declaration = text.match(new RegExp(`\\n\\s*${method}\\(ctx context.Context[^\\n]+`))?.[0] ?? "";
      protocols.set(match[1], { stream: declaration.includes("StreamingClient["), declaration });
    }
  }
}
for (const section of sections) {
  test(`${section.name} operations match the shipped agent protocol`, () => {
    for (const operation of section.operations) {
      const path = `/${operation.service}/${operation.method}`;
      const method = protocols.get(path);
      assert.ok(method, `Unknown operation ${path}`);
      assert.equal(Boolean(operation.stream), method.stream, `${path}: streaming mismatch (${method.declaration})`);
      assert.ok(!operation.automatic || operation.read, `${path}: opening a page must not mutate the device`);
    }
  });
}
