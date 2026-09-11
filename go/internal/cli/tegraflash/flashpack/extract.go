package flashpack

import (
	"github.com/wendylabsinc/wendy/go/internal/cli/archive"
)

// extractZstTar extracts a .tar.zst into dest (created fresh), guarding against
// path traversal. The shared implementation is used by every archive wendy
// downloads, so the guards live in one place.
func extractZstTar(tarball, dest string) error {
	return archive.ExtractTarZst(tarball, dest)
}
