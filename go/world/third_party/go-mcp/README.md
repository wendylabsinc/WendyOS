# Go MCP dependencies

This directory retains license and copyright notices for the Go MCP SDK and its dependencies used by the private `world-mcp` binary. The original license files were copied from the local Go module cache on November 11, 2026, without changes. `sources.json` records every copied file's module version, original location and SHA-256. `COPYRIGHTS.md` retains additional notices found in non-test source files.

| Module | Version | Use |
| --- | --- | --- |
| github.com/modelcontextprotocol/go-sdk | v1.8.0 | Runtime SDK |
| github.com/google/jsonschema-go | v0.4.3 | Runtime dependency |
| github.com/segmentio/asm | v1.1.3 | Runtime dependency |
| github.com/segmentio/encoding | v0.5.4 | Runtime dependency |
| github.com/yosida95/uritemplate/v3 | v3.0.2 | Runtime dependency |
| golang.org/x/oauth2 | v0.35.0 | Runtime dependency |
| golang.org/x/sync | v0.20.0 | Runtime dependency |
| golang.org/x/time | v0.15.0 | Runtime dependency |
| golang.org/x/sys | v0.46.0 | Existing runtime dependency, also linked here |
| github.com/golang-jwt/jwt/v5 | v5.3.1 | Module test/tool dependency, not linked into world-mcp |
| golang.org/x/tools | v0.42.0 | Module test/tool dependency, not linked into world-mcp |

Runtime membership was checked with `GOTOOLCHAIN=go1.27.3 go list -deps` for `./cmd/world-mcp`. The two test/tool modules have new entries in `go.sum`; their notices are retained separately from the runtime claim. The Go 1.27.3 license is included as `GO-LICENSE`, including for Go-derived code identified in Segment's implementation. Segment's separate fuzz-asset license and the complete copyright/permission comment for the Go tools module's goyacc are retained, though neither is linked into this binary.

The SDK's upstream LICENSE describes a transition from MIT to Apache-2.0 and retains the applicable MIT terms. It also identifies a separate license for documentation. Its complete original license text is preserved here; this directory does not relabel the SDK as exclusively MIT or exclusively Apache-2.0. No SDK documentation or specification text is vendored by this change.

The runtime image places these notices at `/notices/go-mcp`. Distributions of the standalone `world-mcp` binary should include this directory as accompanying notices. These notices apply to the named third-party components; they do not change Wendy World's proprietary license or publish its source.
