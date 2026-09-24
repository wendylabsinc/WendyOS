# Documentation build

Run `npm ci`, then `npm run types:check` and `npm run build` from `docs/`.
The build also needs the Go version in the repository's `go.mod`. On macOS,
use the CLI's native build prerequisites, including libusb. Linux needs `libusb-1.0-0-dev`, `libasound2-dev`, and `pkg-config`.
Reference generation uses the native Go compiler with CGO enabled.

`prepare-content.mjs` preserves existing `.md` routes under `/advanced` and
`.mdx` routes at the site root. It creates `/reference/cli` from
`go/cmd/api-surface-snapshot`, which reads the Cobra command tree without
executing commands. Edit command definitions to change syntax, descriptions,
flags, and defaults. Edit the linked hand-written pages for behavioral details.
The generated CLI pages cover public commands; hidden commands retain their
workflow documentation. Platform-specific command availability follows the
build host, Linux in deployment CI.

`publish-markdown.mjs` publishes each page as Markdown, plus `llms.txt` and
`llms-full.txt`. The raw pages retain MDX components and source-relative links;
the index includes the rendered canonical URL for each page. App schema and
CLI JSON are copied from source during preparation. No generated content is
checked in. All public files include the configured version base path in their
index links.

Run `npm run test:docs` for generator checks. Before publishing, also check the
homepage target tabs, mobile sidebar, a generated command's flags and Details
link, and the copy/raw Markdown actions. CLI output examples should come from
source or a real run; do not invent progress lines, flags, or error identifiers.
