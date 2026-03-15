//go:build !darwin && !linux

package commands

import "github.com/spf13/cobra"

func addOSInstallCmd(_ *cobra.Command) {
	// os install is not supported on this platform.
}

func addOSDownloadCmd(_ *cobra.Command) {
	// os download is not supported on this platform.
}
