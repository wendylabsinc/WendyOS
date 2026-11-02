package commands

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
)

var readOperatorServices = sessionbroker.ReadOperatorServices

func newDeviceMeshServicesCmd() *cobra.Command {
	var mesh, kind string
	cmd := &cobra.Command{
		Use:   "services",
		Short: "Browse currently routed services in this operator's mesh",
		Long:  "Read public service descriptions from the current profile's authenticated operator attachment. Services inherit their device manifest's revision and expiry and are shown only while the reporter has a usable authorized route. This does not select or reconfigure a device. Use --json to retain exact owner selectors and service metadata.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			view, err := readOperatorServices(cmd.Context(), sessionbroker.OperatorServiceQuery{Mesh: mesh, Type: kind})
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(view)
			}
			if view.State != "ready" {
				return fmt.Errorf("mesh service inventory is %s; its attached peer must support service observations", view.State)
			}
			if len(view.Services) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "No currently routed services.")
				return err
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			if _, err := fmt.Fprintln(writer, "OWNER\tMESH\tAPP / SERVICE\tTYPE\tPORT\tLEASE"); err != nil {
				return err
			}
			for _, service := range view.Services {
				owner := service.Name
				if owner == "" {
					owner = service.Selector
				}
				if _, err := fmt.Fprintf(writer, "%s\t%s\t%s / %s\t%s\t%d\t%s\n", discovery.SanitiseDisplayName(owner), service.Mesh, service.App, service.Service, service.Type, service.Port, time.Until(time.UnixMilli(service.Expires)).Round(time.Second)); err != nil {
					return err
				}
			}
			if err := writer.Flush(); err != nil {
				return err
			}
			if view.Truncated {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "More services are available; narrow --mesh or --type.")
			}
			return err
		},
	}
	cmd.Flags().StringVar(&mesh, "mesh", "", "Filter the service mesh namespace")
	cmd.Flags().StringVar(&kind, "type", "", "Filter an exact service type such as _http._tcp")
	return cmd
}
