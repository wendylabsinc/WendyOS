# API manifest

`cmd/wendy-core-docs snapshot` produces the stable intermediate `api.json` used by documentation and compatibility tooling. Each release snapshot is stored under `documentation/versions`.

## Versioning

- `schemaVersion` versions the JSON shape, independently of the SDK.
- `version` identifies the SDK contract snapshot.
- Readers must reject unsupported schema versions rather than guessing.
- Arrays are sorted by fully qualified symbol name; field and method order follows the protobuf descriptor.
- No generation timestamp is included, so identical contracts produce byte-identical snapshots.

## Sections

- `contract` links inherited ownership, concurrency, streaming, error, buffer, and projection rules.
- `c.functions` records Clang-normalized signatures, parameters, documentation, and structured operation metadata.
- `protobuf.messages` and `protobuf.enums` record wire values by stable field/value number.
- `protobuf.services` records operation request/response types, streaming shape, C function, and binding pattern.

Clang extracts the C declaration rather than relying on a handwritten symbol list. `protoc` supplies the descriptor and source comments. This keeps the first implementation small while leaving the manifest independent of Doxygen's rendering format.

## Compatibility

The initial `compare` command reports additions, removals, and changed signatures/shapes. Policy layers can classify those findings as:

- C ABI: additions are generally compatible; removals or signature changes are breaking.
- Protobuf wire: new field numbers are generally compatible; number reuse or wire-type changes are breaking.
- Behavior and projection: changes require review against the linked contract and binding pattern.

The current comparator is intentionally conservative and does not claim that every reported protobuf shape change is wire-breaking.
