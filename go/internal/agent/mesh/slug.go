package mesh

import "github.com/wendylabsinc/wendy/go/internal/shared/meshname"

// Normalize canonicalizes a device name or org name to a single DNS label:
// lowercase, every run of non-[a-z0-9] characters collapsed to one hyphen,
// and leading/trailing hyphens trimmed. It is the single source of truth for
// how friendly names and org slugs are compared across the mesh.
func Normalize(s string) string { return meshname.Normalize(s) }
