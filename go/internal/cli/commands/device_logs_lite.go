package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

type liteConsoleProvider interface {
	StreamConsole(context.Context, models.ExternalDevice, func(liteclient.ConsoleChunk) error) error
}

func runLiteDeviceLogs(cmd *cobra.Command, target *SelectedDevice, app string) error {
	if app != "" {
		return fmt.Errorf("Wendy Lite streams a single firmware console; app filtering is not supported")
	}
	for _, flag := range []string{"service", "level", "min-severity", "tail", "no-follow"} {
		if cmd.Flags().Changed(flag) {
			return fmt.Errorf("--%s is not supported by the Wendy Lite console; use Ctrl-C to stop streaming", flag)
		}
	}
	provider, ok := target.Provider.(liteConsoleProvider)
	if !ok {
		return fmt.Errorf("selected provider does not support console logs")
	}
	if !jsonOutput {
		fmt.Fprintln(cmd.ErrOrStderr(), "Streaming Wendy Lite console logs. Press Ctrl-C to stop.")
	}
	return provider.StreamConsole(cmd.Context(), *target.External, func(chunk liteclient.ConsoleChunk) error {
		return writeLiteConsoleChunk(cmd, chunk, jsonOutput)
	})
}

func writeLiteConsoleChunk(cmd *cobra.Command, chunk liteclient.ConsoleChunk, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			Data   string `json:"data"`
			Stderr bool   `json:"stderr"`
			Gap    bool   `json:"gap"`
		}{string(chunk.Data), chunk.Stderr, chunk.Gap})
	}
	if chunk.Gap {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "[Wendy Lite console buffer overflow: some output was lost]"); err != nil {
			return err
		}
	}
	// Merge both board streams into stdout, like the WendyOS log command.
	_, err := cmd.OutOrStdout().Write(chunk.Data)
	return err
}
