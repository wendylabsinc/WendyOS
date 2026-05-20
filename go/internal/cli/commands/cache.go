package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/internal/cli/tui"
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
	type cacheEntry struct {
		Name      string `json:"name"`
		Path      string `json:"path"`
		SizeBytes int64  `json:"sizeBytes"`
		Size      string `json:"size"`
	}

	printJSON := func(items []cacheEntry) error {
		if items == nil {
			items = []cacheEntry{}
		}
		data, err := json.MarshalIndent(items, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	return &cobra.Command{
		Use:   "list",
		Short: "List cached items",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			explicitJSON := jsonOutput && cmd.Root().PersistentFlags().Changed("json")

			cacheDir, err := config.CacheDir()
			if err != nil {
				return err
			}

			entries, err := os.ReadDir(cacheDir)
			if err != nil {
				if os.IsNotExist(err) {
					if explicitJSON {
						return printJSON(nil)
					}
					fmt.Println("Cache is empty.")
					return nil
				}
				return fmt.Errorf("reading cache directory: %w", err)
			}

			if len(entries) == 0 {
				if explicitJSON {
					return printJSON(nil)
				}
				fmt.Println("Cache is empty.")
				return nil
			}

			// Compute sizes up front (needed for both modes).
			// The os-images directory is expanded so each image is listed individually.
			var items []cacheEntry
			for _, entry := range entries {
				if isCacheDBFile(entry.Name()) {
					continue
				}
				path := filepath.Join(cacheDir, entry.Name())
				if entry.IsDir() && entry.Name() == "os-images" {
					imgs, err := os.ReadDir(path)
					if err != nil {
						return fmt.Errorf("reading os-images cache directory: %w", err)
					}
					for _, img := range imgs {
						if img.IsDir() {
							continue
						}
						imgPath := filepath.Join(path, img.Name())
						imgInfo, err := img.Info()
						if err != nil {
							return fmt.Errorf("reading os-images cache entry info for %q: %w", img.Name(), err)
						}
						items = append(items, cacheEntry{
							Name:      "os-images/" + img.Name(),
							Path:      imgPath,
							SizeBytes: imgInfo.Size(),
							Size:      formatSize(imgInfo.Size()),
						})
					}
					continue
				}
				size, err := entrySize(path)
				if err != nil {
					return fmt.Errorf("determining cache entry size for %s: %w", entry.Name(), err)
				}
				items = append(items, cacheEntry{
					Name:      entry.Name(),
					Path:      path,
					SizeBytes: size,
					Size:      formatSize(size),
				})
			}

			if explicitJSON {
				return printJSON(items)
			}

			// Interactive mode when stdin and stdout are both terminals.
			if isInteractiveTerminal() {
				checkItems := make([]tui.ChecklistItem, len(items))
				for i, item := range items {
					checkItems[i] = tui.ChecklistItem{
						Label:       item.Name,
						Description: item.Size,
						Value:       item.Path,
					}
				}

				cl := tui.NewChecklist("Select cache entries to delete:", checkItems)
				cl.SelectAllLabel = "Delete all"
				selected, err := tui.RunChecklistModel(cl, tea.WithOutput(os.Stderr))
				if err != nil {
					if errors.Is(err, tui.ErrCancelled) {
						return nil
					}
					return err
				}
				if len(selected) == 0 {
					return nil
				}

				confirmed, err := tui.Confirm(fmt.Sprintf("Delete %d item(s)?", len(selected)), tea.WithOutput(os.Stderr))
				if err != nil {
					if errors.Is(err, tui.ErrCancelled) {
						return nil
					}
					return err
				}
				if !confirmed {
					return nil
				}

				for _, item := range selected {
					if err := os.RemoveAll(item.Value); err != nil {
						fmt.Fprintf(os.Stderr, "error: removing %s: %v\n", item.Label, err)
					} else {
						fmt.Printf("Deleted %s\n", item.Label)
					}
				}
				return nil
			}

			// Non-interactive (plain listing).
			for _, item := range items {
				fmt.Printf("  %s  (%s)\n", item.Name, item.Size)
			}
			return nil
		},
	}
}

// isCacheDBFile returns true for SQLite database files that back the CLI cache
// and must not be removed while the process is running.
func isCacheDBFile(name string) bool {
	switch name {
	case "Cache.db", "Cache.db-shm", "Cache.db-wal":
		return true
	}
	return false
}

func entrySize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.1f GB", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.1f MB", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.1f KB", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%d B", bytes)
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
