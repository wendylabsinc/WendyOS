package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// The native apps use this bounded, noninteractive surface. Discovery always
// runs on the chosen gateway, never silently on the desktop's different LAN.
func newDeviceSensorsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sensors", Short: "Discover and connect SensorLink sources on a gateway"}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List sensor sources reachable from the selected gateway", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			cmd.SetContext(ctx)
			conn, err := connectToAgent(cmd.Context(), NonInteractive(), SuppressProvisioningHint())
			if err != nil {
				return err
			}
			defer conn.Close()
			response, err := conn.SensorPairingService.DiscoverSensorSources(cmd.Context(), &agentpbv2.DiscoverSensorSourcesRequest{TimeoutMs: 5000})
			if err != nil {
				return fmt.Errorf("discovering sensors on the gateway: %w", err)
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(response.GetSources())
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "status", Short: "List saved SensorLink pairings and their observed connection state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			cmd.SetContext(ctx)
			conn, err := connectToAgent(cmd.Context(), NonInteractive(), SuppressProvisioningHint())
			if err != nil {
				return err
			}
			defer conn.Close()
			response, err := conn.SensorPairingService.ListSensorPairings(cmd.Context(), &agentpbv2.ListSensorPairingsRequest{})
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
		},
	})
	var principal string
	add := &cobra.Command{
		Use: "add", Short: "Connect an enrolled source by its exact advertised PKI principal", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			cmd.SetContext(ctx)
			if principal == "" {
				return fmt.Errorf("--source is required; choose a principal_uri from 'device sensors list'")
			}
			conn, err := connectToAgent(cmd.Context(), NonInteractive(), SuppressProvisioningHint())
			if err != nil {
				return err
			}
			defer conn.Close()
			handler := &sensorPairHandler{ctx: cmd.Context(), client: conn.SensorPairingService, orgIDs: cliOrgIDs, tenantIDs: cliTenantIDs}
			sources, _, err := handler.discoverSources(cmd.Context())
			if err != nil {
				return err
			}
			source, err := selectSensorPrincipal(sources, principal)
			if err != nil {
				return err
			}
			result, ok := handler.pair(source)().(sensorPairOpMsg)
			if !ok {
				return fmt.Errorf("unexpected sensor pairing result")
			}
			if result.err != nil {
				return result.err
			}
			// Read back persisted state. The interactive handler can synthesize a
			// display row for an older server; that is not confirmation for an app.
			stored, err := conn.SensorPairingService.ListSensorPairings(cmd.Context(), &agentpbv2.ListSensorPairingsRequest{})
			if err != nil {
				return fmt.Errorf("verifying saved sensor pairing: %w", err)
			}
			for _, pairing := range stored.GetPairings() {
				if pairing.GetSourcePrincipalUri() == principal {
					// Connected may still be false until sensor data arrives.
					return json.NewEncoder(cmd.OutOrStdout()).Encode(pairing)
				}
			}
			return fmt.Errorf("the gateway did not confirm the saved sensor pairing")
		},
	}
	add.Flags().StringVar(&principal, "source", "", "Exact principal_uri from device sensors list")
	cmd.AddCommand(add)
	return cmd
}

func selectSensorPrincipal(sources []models.DiscoveredDevice, principal string) (models.DiscoveredDevice, error) {
	var selected models.DiscoveredDevice
	matches := 0
	for _, source := range sources {
		if principal != "" && source.IsMTLS && sensorSourcePrincipal(source) == principal {
			selected = source
			matches++
		}
	}
	if matches != 1 {
		return selected, fmt.Errorf("sensor principal must match exactly one enrolled source on the gateway network; found %d", matches)
	}
	return selected, nil
}
