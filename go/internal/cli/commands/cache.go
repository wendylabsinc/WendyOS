package commands

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/shared/config"
)

func newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Manage local CLI cache",
	}

	cmd.AddCommand(
		newCacheListCmd(),
		newCacheClearCmd(),
	)

	return cmd
}

func newCacheListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List cached items",
		RunE: func(cmd *cobra.Command, args []string) error {
			cacheDir, err := config.CacheDir()
			if err != nil {
				return err
			}

			entries, err := os.ReadDir(cacheDir)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Println("Cache is empty.")
					return nil
				}
				return fmt.Errorf("reading cache directory: %w", err)
			}

			if len(entries) == 0 {
				fmt.Println("Cache is empty.")
				return nil
			}

			for _, entry := range entries {
				info, err := entry.Info()
				if err != nil {
					continue
				}
				fmt.Printf("  %s  (%d bytes)\n", entry.Name(), info.Size())
			}

			return nil
		},
	}
}

func newCacheClearCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "Clear the local cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			cacheDir, err := config.CacheDir()
			if err != nil {
				return err
			}

			if err := os.RemoveAll(cacheDir); err != nil {
				return fmt.Errorf("clearing cache: %w", err)
			}

			fmt.Println("Cache cleared.")
			return nil
		},
	}
}
