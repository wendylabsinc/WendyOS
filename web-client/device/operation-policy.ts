import type { Operation } from "./operations";
import type { Field } from "./rpc";

export function operationInput(operation: Operation, input: Record<string, unknown>) {
  // Preview is a fixed read-only action, regardless of any edited form values.
  return operation.read && operation.method === "PruneCache" ? { ...input, dryRun: true } : input;
}
export function operationFields(operation: Operation, fields: Field[]) {
  return operation.read && operation.method === "PruneCache" ? fields.filter((field) => field.name !== "dryRun") : fields;
}
