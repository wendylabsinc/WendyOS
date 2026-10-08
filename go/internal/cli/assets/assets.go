// Package assets embeds Wendy documentation and AI skill files for offline use.
package assets

import "embed"

// FS contains documentation and skill files embedded at compile time.
// Most of the docs-site source (Next.js app, components, images, video) lives
// in the docs tree but is intentionally excluded to keep the CLI binary
// small. The MDX content directories below (guides, hardware, installation,
// integrations, remote-debugging, security) are text-only reference/guide
// content — no images or Next.js machinery — so they are embedded alongside
// the plain-Markdown reference docs, making them available to `wendy docs`
// and the MCP wendy://docs/ resources with no network access required.
//
// docs/index.mdx (the marketing landing page) is deliberately excluded: it
// leans on interactive components (<Cards>, <HardwareShowcase>, nested JSX in
// attributes) that don't degrade to readable plain text the way the
// reference/guide content does.
//
//go:embed docs/Examples docs/apps docs/architecture docs/clients docs/cloud docs/debugging docs/development docs/pki docs/vscode docs/wendy-lite docs/wendy-os-publisher docs/wendyos docs/RELEASES.md docs/device/entitlements.md docs/roadmap.md docs/guides docs/hardware docs/installation docs/integrations docs/remote-debugging docs/security skills mcp-skills chatgpt-plugin
var FS embed.FS
