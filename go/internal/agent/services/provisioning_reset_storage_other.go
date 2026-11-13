//go:build !unix

package services

import "os"

const supportsTrustedResetStorage = false

// No equivalent protected-state ownership verifier exists on this platform.
// Refuse pending erasure rather than trusting caller-writable saved keys.
func trustedResetStorage(os.FileInfo, os.FileInfo) bool { return false }
