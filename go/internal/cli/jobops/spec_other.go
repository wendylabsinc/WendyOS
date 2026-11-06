//go:build !unix

package jobops

import "os"

const regularReadFlags = 0

func openSpec(name string) (*os.File, error) { return os.Open(name) }
