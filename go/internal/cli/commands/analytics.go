package commands

import (
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
)

func newAnalyticsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "analytics",
		Short: "Manage anonymous usage analytics",
	}

	cmd.AddCommand(
		newAnalyticsEnableCmd(),
		newAnalyticsDisableCmd(),
		newAnalyticsStatusCmd(),
	)

	return cmd
}

func newAnalyticsEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable",
		Short: "Enable anonymous usage analytics",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			cfg.Analytics = &config.AnalyticsConfig{Enabled: true}
			if err := config.Save(cfg); err != nil {
				return err
			}

			// Apply the preference to this invocation too. Init still enforces
			// environment/CI overrides and handles an unusable identity file.
			analytics.Init(cfg)
			cmd.Println("Analytics enabled.")
			return nil
		},
	}
}

func newAnalyticsDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Disable anonymous usage analytics",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			cfg.Analytics = &config.AnalyticsConfig{Enabled: false}
			if err := config.Save(cfg); err != nil {
				return err
			}

			// Disable analytics in-memory for the current process so that
			// no further events are emitted during this invocation.
			analytics.Disable()

			cmd.Println("Analytics disabled.")
			return nil
		},
	}
}

func newAnalyticsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show current analytics status",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			if env.IsCI() {
				cmd.Println("Analytics: disabled (CI environment detected)")
				return nil
			}
			if analytics.EnvOverride() {
				cmd.Println("Analytics: disabled (overridden by WENDY_ANALYTICS=false)")
				return nil
			}

			if cfg.Analytics != nil && !cfg.Analytics.Enabled {
				cmd.Println("Analytics: disabled")
			} else if !analytics.Enabled() {
				cmd.Println("Analytics: disabled (analytics initialization failed)")
			} else {
				cmd.Println("Analytics: enabled")
			}

			return nil
		},
	}
}
