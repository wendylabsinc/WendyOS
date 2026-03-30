//go:build darwin || linux

package commands

import "github.com/spf13/cobra"

func addOSInstallCmd(parent *cobra.Command) {
	parent.AddCommand(newOSInstallCmd())
}

func addOSDownloadCmd(parent *cobra.Command) {
	parent.AddCommand(newOSDownloadCmd())
}
