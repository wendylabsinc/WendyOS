//go:build windows

package swifttoolchain

import "os"

// unzipOverwriteEnv is a no-op on Windows. The unix wrapper writes a
// `#!/bin/sh` shim that invokes `/usr/bin/unzip` — neither shebang
// interpretation nor that binary exist on Windows, and `swift sdk install`'s
// duplicate-entry prompt is a unix-only concern. Returns the current
// environment unchanged so callers' env/cleanup contract still holds.
func unzipOverwriteEnv() (env []string, cleanup func(), err error) {
	return os.Environ(), func() {}, nil
}
