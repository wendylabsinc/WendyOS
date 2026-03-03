package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/shared/version"
)

const githubReleasesURL = "https://api.github.com/repos/wendylabsinc/wendy-agent/releases/latest"

func newUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Check for CLI updates",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("Current version: %s\n", version.Version)
			fmt.Println("Checking for updates...")

			latest, err := checkLatestRelease()
			if err != nil {
				return fmt.Errorf("checking for updates: %w", err)
			}

			if latest == version.Version {
				fmt.Println("You are running the latest version.")
				return nil
			}

			fmt.Printf("A new version is available: %s\n", latest)
			fmt.Println("Update with: brew upgrade wendy")
			return nil
		},
	}
}

type githubRelease struct {
	TagName string `json:"tag_name"`
}

func checkLatestRelease() (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Get(githubReleasesURL)
	if err != nil {
		return "", fmt.Errorf("fetching releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("decoding release: %w", err)
	}

	return release.TagName, nil
}
