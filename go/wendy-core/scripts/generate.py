"""Generate the shared JS operation surface from the canonical C manifest."""
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
manifest = json.loads((ROOT / ".build/sdk/api.json").read_text())
if manifest["schemaVersion"] != 1:
    raise ValueError("Unsupported API manifest schema")
# Async/object operations are consumed by the native SDKs. The bootstrap JS
# projection remains scalar/string-only until it has host callbacks and ownership.
native_patterns = {"async-unary", "owned-object"}
functions = [function for function in manifest["c"]["functions"]
             if function.get("pattern") not in native_patterns]
TYPES = {"int32_t (void)": "number", "char *(void)": "string"}
if any(function["signature"] not in TYPES for function in functions):
    raise ValueError("Add an explicit projection rule before exposing a new C signature")
operations = "\n".join(
    f'    {function["name"]}() {{ return backend.{function["name"]}(); }},' for function in functions
)
source = "// Generated from the C contract. Run make generate; do not edit.\n"
(ROOT / "distributions/javascript/src/interface.mjs").write_text(
    source + "export function createCore(backend) {\n  return Object.freeze({\n" + operations + "\n  });\n}\n"
)
(ROOT / "distributions/javascript/src/index.d.ts").write_text(
    source + "\n".join(f'/** {function["comment"]} */\nexport declare function {function["name"]}(): {TYPES[function["signature"]]};' for function in functions) + "\n"
)
