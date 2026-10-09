package commands

import (
	"context"
	"github.com/spf13/cobra"
	"testing"
)

func TestCloudEnrollmentUsesLocalTarget(t *testing.T) {
	for _, cmd := range []*cobra.Command{newDeviceEnrollCmd(), newCloudEnrollDeviceCmd()} {
		t.Run(cmd.Name(), func(t *testing.T) {
			cmd.SetContext(context.Background())
			cmd.RunE = func(cmd *cobra.Command, _ []string) error {
				if _, ok := cloudDeviceConfigFromContext(cmd.Context()); ok {
					t.Fatal("enrollment was routed through cloud agent")
				}
				return nil
			}
			wrapCloudDeviceCommands(cmd, func(*cobra.Command) cloudDeviceConfig { return cloudDeviceConfig{DeviceName: "wendy-lite:/dev/test"} })
			if err := cmd.RunE(cmd, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
	cmd := &cobra.Command{Use: "logs"}
	cmd.SetContext(context.Background())
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if _, ok := cloudDeviceConfigFromContext(cmd.Context()); !ok {
			t.Fatal("ordinary cloud command lost its route")
		}
		return nil
	}
	wrapCloudDeviceCommands(cmd, func(*cobra.Command) cloudDeviceConfig { return cloudDeviceConfig{} })
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
}
