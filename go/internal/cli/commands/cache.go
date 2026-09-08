package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Manage local CLI cache",
	}

	cmd.AddCommand(
		newCacheListCmd(),
		newCacheClearCmd(),
		newCacheDedupCmd(),
	)

	return cmd
}

func newCacheDedupCmd() *cobra.Command {
	var maxBytes int64
	cmd := &cobra.Command{
		Use:   "dedup",
		Short: "Reclaim build-cache space (hardlink-dedup identical layers, evict old app caches)",
		Long: "Hardlinks content-identical layers shared across the buildx and " +
			"ocilayout stores so each layer costs one copy, then evicts the " +
			"least-recently-used per-app caches until the total is under --max. " +
			"Only ever dedups or deletes cache content; safe to run anytime.\n\n" +
			"This runs automatically after every build, so the caches stay bounded " +
			"without ever invoking it by hand — it exists for manual reclaim and debugging.",
		// Hidden: the automatic post-build maintenance is the intended path; this
		// stays available for on-demand reclaim without cluttering `wendy cache`.
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			userCache, err := os.UserCacheDir()
			if err != nil {
				return fmt.Errorf("finding user cache directory: %w", err)
			}
			deduped, pruned := maintainBuildCaches(cmd.Context(), userCache, maxBytes, nil)
			fmt.Printf("Deduplicated %s, evicted %s.\n", formatSize(deduped), formatSize(pruned))
			return nil
		},
	}
	cmd.Flags().Int64Var(&maxBytes, "max", buildCacheMaxBytes(),
		"evict oldest app caches above this many bytes (0 to dedup only)")
	return cmd
}

func newCacheListCmd() *cobra.Command {
	type cacheEntry struct {
		Name      string `json:"name"`
		Path      string `json:"path"`
		SizeBytes int64  `json:"sizeBytes"`
		Size      string `json:"size"`
		LastBuilt string `json:"lastBuilt,omitempty"`
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
			// jsonOutput is auto-enabled for non-interactive commands; cache list
			// keeps plain text there unless --json was explicitly requested.
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

			// Compute sizes up front (needed for both modes). os-images, buildx and
			// ocilayout are expanded so each image / per-app cache is listed
			// individually; the build caches also carry a last-built age.
			now := time.Now()
			var items []cacheEntry
			var buildRoots []string
			var sharedBytes int64
			for _, entry := range entries {
				if isCacheDBFile(entry.Name()) {
					continue
				}
				path := filepath.Join(cacheDir, entry.Name())
				switch {
				case entry.IsDir() && entry.Name() == "os-images":
					imgs, err := os.ReadDir(path)
					if err != nil {
						return fmt.Errorf("reading os-images cache directory: %w", err)
					}
					for _, img := range imgs {
						imgPath := filepath.Join(path, img.Name())
						// Files report their own size; directories (e.g. an extracted
						// Thor flashpack tree) are sized recursively so they show up and
						// can be selected for deletion like any other cache entry.
						var sz int64
						if img.IsDir() {
							s, err := entrySize(imgPath)
							if err != nil {
								return fmt.Errorf("sizing os-images cache entry %q: %w", img.Name(), err)
							}
							sz = s
						} else {
							imgInfo, err := img.Info()
							if err != nil {
								return fmt.Errorf("reading os-images cache entry info for %q: %w", img.Name(), err)
							}
							sz = imgInfo.Size()
						}
						items = append(items, cacheEntry{
							Name:      "os-images/" + img.Name(),
							Path:      imgPath,
							SizeBytes: sz,
							Size:      formatSize(sz),
						})
					}
				case entry.IsDir() && (entry.Name() == "buildx" || entry.Name() == "ocilayout"):
					children, err := os.ReadDir(path)
					if err != nil {
						return fmt.Errorf("reading %s cache directory: %w", entry.Name(), err)
					}
					buildRoots = append(buildRoots, path)
					for _, c := range children {
						cp := filepath.Join(path, c.Name())
						// Per-row size is per-link: after dedup a layer shared by N apps
						// shows in each of their rows, so rows can sum past the real
						// on-disk total reported in the summary below.
						sz, mt := dirSizeAndMtime(cp)
						// blobs/, ingest/ and the top-level index files are the shared
						// store, not a per-app cache: fold them into the summary rather
						// than list a row that can't be deleted without taking the app
						// caches down with it.
						if !c.IsDir() || c.Name() == "blobs" || c.Name() == "ingest" {
							sharedBytes += sz
							continue
						}
						items = append(items, cacheEntry{
							Name:      entry.Name() + "/" + c.Name(),
							Path:      cp,
							SizeBytes: sz,
							Size:      formatSize(sz),
							LastBuilt: formatAge(now, mt),
						})
					}
				default:
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
			}
			// Largest first — the whole point of listing is to find what to reclaim.
			sort.Slice(items, func(i, j int) bool { return items[i].SizeBytes > items[j].SizeBytes })

			// The headline number is the dedup-aware on-disk total — the same
			// accounting the size cap is enforced against — so "over cap" here
			// means the next build's maintenance pass will evict something.
			buildCacheSummary := ""
			if onDisk := scanBuildCache(nil, buildRoots...).total; onDisk > 0 {
				buildCacheSummary = fmt.Sprintf("Build cache: %s on disk (%s shared) · cap %s",
					formatSize(onDisk), formatSize(sharedBytes), formatSize(buildCacheMaxBytes()))
			}

			if explicitJSON {
				return printJSON(items)
			}

			// Interactive mode when stdin and stdout are both terminals.
			if isInteractiveTerminal() {
				checkItems := make([]tui.ChecklistItem, len(items))
				for i, item := range items {
					desc := item.Size
					if item.LastBuilt != "" {
						desc += "  ·  built " + item.LastBuilt
					}
					checkItems[i] = tui.ChecklistItem{
						Label:       item.Name,
						Description: desc,
						Value:       item.Path,
					}
				}

				title := "Select cache entries to delete:"
				if buildCacheSummary != "" {
					title = buildCacheSummary + "\nSelect cache entries to delete:"
				}
				cl := tui.NewChecklist(title, checkItems)
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
				if item.LastBuilt != "" {
					fmt.Printf("  %s  (%s, built %s)\n", item.Name, item.Size, item.LastBuilt)
				} else {
					fmt.Printf("  %s  (%s)\n", item.Name, item.Size)
				}
			}
			if buildCacheSummary != "" {
				fmt.Printf("\n%s\n", buildCacheSummary)
			}
			return nil
		},
	}
}

// formatAge renders how long ago t was, coarsely (m/h/d). Empty for a zero time.
func formatAge(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
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
