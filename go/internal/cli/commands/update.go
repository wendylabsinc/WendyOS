package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/version"
)

const githubReleasesURL = "https://api.github.com/repos/wendylabsinc/wendy-agent/releases/latest"

const cliUpdateCheckInterval = 24 * time.Hour

// cliUpdateNoticeCh receives the latest version string when a background update
// check finds a newer release. Buffered so the goroutine never blocks.
var cliUpdateNoticeCh = make(chan string, 1)

// scheduleCLIUpdateCheck launches a goroutine that fetches the latest release.
// The timestamp is written only after the network call completes so that a
// fast process exit (goroutine killed before it finishes) doesn't burn the 24 h
// window. If a newer version is found it sends it to cliUpdateNoticeCh for
// PersistentPostRunE to display.
func scheduleCLIUpdateCheck(cfg *config.Config) {
	go func() {
		latest, err := checkLatestRelease()
		// Persist the timestamp regardless of outcome so we don't hammer the
		// API on repeated fast invocations when the network is unavailable.
		cfg.LastCLIUpdateCheck = time.Now().UTC().Format(time.RFC3339)
		if saveErr := config.Save(cfg); saveErr != nil {
			// If we can't persist the timestamp, bail out so we retry next time.
			return
		}
		if err != nil {
			return
		}
		if version.CompareVersions(latest, version.Version) > 0 {
			select {
			case cliUpdateNoticeCh <- latest:
			default:
			}
		}
	}()
}

// dueCLIUpdateCheck returns true when the CLI is a released build and enough
// time has passed since the last check.
func dueCLIUpdateCheck(cfg *config.Config) bool {
	if version.Version == "dev" {
		return false
	}
	if cfg.LastCLIUpdateCheck == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, cfg.LastCLIUpdateCheck)
	if err != nil {
		return true
	}
	now := time.Now().UTC()
	if t.After(now) {
		// Stored timestamp is in the future (clock skew or manual edit); treat as due.
		return true
	}
	return now.Sub(t) >= cliUpdateCheckInterval
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
