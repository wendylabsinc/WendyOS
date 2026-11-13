# Published relay catalogs

These JSON files are immutable snapshots of reviewed, published relay tool catalogs.
The source catalog is `service-protos/docs/relay-mcp-tools.json`; Wendy Cloud bundles
the same file as `swift/Sources/GRPCServices/relay-mcp-tools.json`.

`7d3624a60bf68a8c.json` was copied from the deployed Cloud bundle with version
`2026.10.13-212424+7d3624a60bf68a8c` and contains 58 tools. Each filename is the
catalog's tool-definition hash. The resolver verifies both the version suffix and
the definitions against that hash. Do not edit an existing snapshot or replace its
definitions while keeping its hash.

When publishing a new catalog, retain every previously shipped or published catalog
that the CLI still supports, and add the new published snapshot here. This lets
subsequent CLI releases recognize relays still serving earlier catalogs. Generate
new catalogs with `wendy mcp manifest` and keep the service-protos source and
Cloud bundle identical; do not construct catalogs from relay-supplied definitions.

The CLI accepts its current catalog directly. For an older bundled catalog, it
checks that tools remain available, annotations remain unchanged, and input-schema
changes are supported additions. Each session lists the selected catalog and
rejects calls outside its tool names and input schema, including newly added
top-level arguments. Unknown catalogs are refused.

Schema checks cannot infer changes in handler behavior. Reviewers must preserve
the behavior of older calls, including defaults and side effects, or explicitly
retire the affected catalog. A retired or incompatible catalog refuses pairing
until the CLI and relay have a compatible published catalog again. Add regression
coverage for any older behavior that a new implementation could change.
