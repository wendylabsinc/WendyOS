//go:build !darwin && !linux && !windows

package commands

import "github.com/spf13/cobra"

func addOSCacheCmd(_ *cobra.Command) {
	// os cache is not supported on this platform.
}
