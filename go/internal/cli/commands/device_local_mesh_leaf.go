package commands

import (
	"fmt"
	"net"
	"strconv"

	"github.com/spf13/cobra"
)

// localMeshDefaultLeafListen is where Wendy Lite boards look for a router.
// Lite firmware dials TCP port 43024 (CONFIG_WENDY_MESH_ROUTER_PORT) on every
// router it discovers over mDNS, and no advertisement carries another port.
// The agent's localmesh.LeafPort is the same number.
const localMeshDefaultLeafListen = "0.0.0.0:43024"

// addLocalMeshLeafFlags registers the Wendy Lite leaf listener options. They
// live apart from the carrier booleans so another configure command can reuse
// them unchanged.
func addLocalMeshLeafFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("leaf", false, "Accept Wendy Lite boards as net endpoints on TCP "+localMeshDefaultLeafListen+" (false stops accepting them)")
	cmd.Flags().String("leaf-listen", "", "Accept Wendy Lite boards on this TCP host:port instead of "+localMeshDefaultLeafListen)
}

// localMeshLeafListen returns the leaf_listen update, or nil when neither leaf
// option was given so the saved value is preserved. An empty string disables.
func localMeshLeafListen(cmd *cobra.Command) (*string, error) {
	leafSet, addressSet := cmd.Flags().Changed("leaf"), cmd.Flags().Changed("leaf-listen")
	switch {
	case leafSet && addressSet:
		return nil, fmt.Errorf("use either --leaf or --leaf-listen, not both")
	case addressSet:
		address, err := cmd.Flags().GetString("leaf-listen")
		if err != nil {
			return nil, err
		}
		// The agent validates again before saving; this only fails fast.
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("--leaf-listen %q: want host:port such as %s; use --leaf=false to stop accepting Wendy Lite boards", address, localMeshDefaultLeafListen)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("--leaf-listen %q: port must be 1-65535", net.JoinHostPort(host, port))
		}
		return &address, nil
	case leafSet:
		enabled, err := cmd.Flags().GetBool("leaf")
		if err != nil {
			return nil, err
		}
		address := ""
		if enabled {
			address = localMeshDefaultLeafListen
		}
		return &address, nil
	}
	return nil, nil
}

func localMeshLeafLabel(address string) string {
	if address == "" {
		return "off"
	}
	return address
}
