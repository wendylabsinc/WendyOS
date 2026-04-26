package commands

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

func newOpenBrowserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "open-browser <url>",
		Short: "Open a URL in the default browser",
		Long:  "Open a URL in the default browser. Works on macOS, Linux, and Windows.\nUseful in wendy.json postStart hooks for cross-platform browser opening.",
		Example: `  wendy utils open-browser http://localhost:3000
  wendy utils open-browser https://example.com`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rawURL := args[0]
			parsed, err := url.ParseRequestURI(rawURL)
			if err != nil {
				return fmt.Errorf("invalid URL %q: %w", rawURL, err)
			}
			if parsed.Scheme == "" {
				return fmt.Errorf("invalid URL %q: missing scheme (e.g. http:// or https://)", rawURL)
			}
			if (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host == "" {
				return fmt.Errorf("invalid URL %q: must include a host (e.g. http://localhost:3000)", rawURL)
			}

			if err := openBrowser(rawURL); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Could not open browser: %v\n", err)
				fmt.Fprintln(cmd.OutOrStdout(), rawURL)
				return nil
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Opening %s in default browser...\n", rawURL)
			return nil
		},
	}

	return cmd
}
