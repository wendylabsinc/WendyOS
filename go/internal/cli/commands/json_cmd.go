package commands

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

func newJSONCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "json",
		Short: "Inspect and validate wendy.json",
	}

	cmd.AddCommand(newJSONSchemaCmd())
	cmd.AddCommand(newJSONValidateCmd())

	return cmd
}

func newJSONSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema for wendy.json",
		Long:  "Prints the JSON Schema to stdout. Pipe to a file or use $schema in your wendy.json for editor autocompletion.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println(appconfig.SchemaJSON)
			return nil
		},
	}
}

func newJSONValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [path]",
		Short: "Validate a wendy.json file",
		Long:  "Validates a wendy.json for required fields, valid entitlement types, and unknown entitlement keys.\nPath can be a file or a directory containing wendy.json. Defaults to the current directory.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := ""
			if len(args) > 0 {
				path = args[0]
			} else {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("getting working directory: %w", err)
				}
				path = cwd
			}

			// If path is a directory, look for wendy.json inside it.
			info, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			if info.IsDir() {
				path = filepath.Join(path, "wendy.json")
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}

			cfg, err := appconfig.LoadFromBytes(data)
			if err != nil {
				return err
			}

			if err := cfg.Validate(); err != nil {
				return err
			}

			warnings := appconfig.ValidateJSON(data)
			printAppConfigWarnings(os.Stderr, warnings)

			if len(warnings) > 0 {
				fmt.Println("wendy.json is valid (with warnings).")
			} else {
				fmt.Println("wendy.json is valid.")
			}
			return nil
		},
	}
}
