package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

const githubReleasesURL = "https://api.github.com/repos/wendylabsinc/wendy-agent/releases/latest"

const cliUpdateCheckInterval = 24 * time.Hour

// scheduleCLIUpdateCheck launches a goroutine that fetches the latest release
// and persists the result to config. PersistentPostRunE reads the persisted
// value on the next invocation, which avoids the race where the HTTP call
// hasn't finished by the time a fast command completes.
func scheduleCLIUpdateCheck() {
	go func() {
		latest, checkErr := checkLatestRelease()
		// Best-effort: if we can't save, we'll retry on the next check.
		_ = persistCLIUpdateCheckResult(time.Now(), latest, checkErr)
	}()
}

// persistCLIUpdateCheckResult reloads config under the refresh lock before
// changing update metadata. The HTTP request can overlap an OAuth refresh; saving
// the config snapshot from before that request would restore the consumed refresh
// token and cause wendy-auth to revoke the rotated token family on its next use.
func persistCLIUpdateCheckResult(checkedAt time.Time, latest string, checkErr error) error {
	unlock, err := acquireAuthRefreshLock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.LastCLIUpdateCheck = checkedAt.UTC().Format(time.RFC3339)
	if checkErr == nil {
		if version.CompareVersions(latest, version.Version) > 0 {
			cfg.AvailableCLIUpdate = latest
		} else {
			cfg.AvailableCLIUpdate = ""
		}
	}
	return config.Save(cfg)
}

// dueCLIUpdateCheck returns true when the CLI is a released build and enough
// time has passed since the last check.
func dueCLIUpdateCheck(cfg *config.Config) bool {
	if version.IsDev(version.Version) {
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
	client := newGitHubAPIClient(10 * time.Second)

	req, err := newGitHubAPIGetRequest(githubReleasesURL)
	if err != nil {
		return "", fmt.Errorf("creating GitHub API request: %w", err)
	}

	resp, err := client.Do(req)
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
