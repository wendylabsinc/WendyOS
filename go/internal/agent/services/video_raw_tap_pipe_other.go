//go:build !linux

package services

import "os"

// tapPipeCapacity is how many bytes the tap pipe holds. Only Linux can be
// asked (F_GETPIPE_SZ); elsewhere it is the 64 KiB a macOS pipe grows to.
func tapPipeCapacity(*os.File) int { return defaultTapPipeCapacity }
