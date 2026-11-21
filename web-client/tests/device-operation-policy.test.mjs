import test from "node:test";
import assert from "node:assert/strict";
import { operationInput, operationFields } from "../device/operation-policy.ts";
const preview = { service: "WendyContainerService", method: "PruneCache", read: true };
test("cache preview cannot become destructive through editable input", () => {
  assert.deepEqual(operationInput(preview, { dryRun: false, all: true }), { dryRun: true, all: true });
  assert.deepEqual(operationInput(preview, {}), { dryRun: true });
  assert.deepEqual(operationFields(preview, [{ name: "dryRun" }, { name: "all" }]), [{ name: "all" }]);
});
test("confirmed cleanup retains its selected behavior", () => {
  const cleanup = { ...preview, read: false };
  assert.deepEqual(operationInput(cleanup, { dryRun: false }), { dryRun: false });
  assert.deepEqual(operationFields(cleanup, [{ name: "dryRun" }]), [{ name: "dryRun" }]);
});
