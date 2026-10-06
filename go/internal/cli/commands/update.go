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
// hasn't finished by the time a fast command completes. It is a variable so
// tests can observe it without a network call.
var scheduleCLIUpdateCheck = func() {
	go checkCLIUpdateIfDue(context.Background())
}

// checkCLIUpdateIfDue is shared by ordinary invocations and the long-lived
// MCP server. Read the current cache each time; an hourly MCP tick only
// contacts GitHub when the existing 24-hour interval has elapsed.
func checkCLIUpdateIfDue(ctx context.Context) {
	if ctx.Err() != nil || runsAsForeignUser() {
		return
	}
	cfg, err := config.Load()
	if err != nil || !dueCLIUpdateCheck(cfg) {
		return
	}
	latest, checkErr := checkLatestReleaseContext(ctx)
	if ctx.Err() != nil {
		return
	}
	_ = persistCLIUpdateCheckResultContext(ctx, time.Now(), latest, checkErr)
}

// persistCLIUpdateCheckResult persists one check's outcome by changing only its
// own two fields on the CURRENT config. It used to save the snapshot root
// loaded at startup, seconds after the command had saved its own changes — a
// new default device, a fresh pin, a login — and silently revert them. It also
// holds the auth refresh lock: the HTTP request can overlap an OAuth refresh,
// and saving a config read before that refresh finished would restore the
// consumed refresh token and cause wendy-auth to revoke the rotated token
// family on its next use.
func persistCLIUpdateCheckResult(checkedAt time.Time, latest string, checkErr error) error {
	return persistCLIUpdateCheckResultContext(context.Background(), checkedAt, latest, checkErr)
}

func persistCLIUpdateCheckResultContext(ctx context.Context, checkedAt time.Time, latest string, checkErr error) error {
	unlock, err := acquireAuthRefreshLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	return config.Update(func(cfg *config.Config) (bool, error) {
		cfg.LastCLIUpdateCheck = checkedAt.UTC().Format(time.RFC3339)
		if checkErr == nil {
			if version.CompareVersions(latest, version.Version) > 0 {
				cfg.AvailableCLIUpdate = latest
			} else {
				cfg.AvailableCLIUpdate = ""
			}
		}
		return true, nil
	})
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
	return checkLatestReleaseContext(context.Background())
}

func checkLatestReleaseContext(ctx context.Context) (string, error) {
	client := newGitHubAPIClient(10 * time.Second)

	req, err := newGitHubAPIGetRequest(githubReleasesURL)
	if err != nil {
		return "", fmt.Errorf("creating GitHub API request: %w", err)
	}
	req = req.WithContext(ctx)

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
