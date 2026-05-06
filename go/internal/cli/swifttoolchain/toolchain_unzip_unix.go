//go:build !windows

package swifttoolchain

import (
	"os"
	"path/filepath"
)

// unzipOverwriteEnv returns a modified env with an unzip wrapper prepended to
// PATH. The wrapper passes -o (overwrite without prompting) to the real unzip
// binary, which prevents interactive prompts when the zip has duplicate
// entries. Call the returned cleanup func when done.
func unzipOverwriteEnv() (env []string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "wendy-unzip-*")
	if err != nil {
		return nil, func() {}, err
	}
	script := "#!/bin/sh\nexec /usr/bin/unzip -o \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "unzip"), []byte(script), 0755); err != nil {
		os.RemoveAll(dir)
		return nil, func() {}, err
	}
	env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return env, func() { os.RemoveAll(dir) }, nil
}
