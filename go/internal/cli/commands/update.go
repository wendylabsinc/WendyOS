package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

const githubReleasesURL = "https://api.github.com/repos/wendylabsinc/wendy-agent/releases/latest"

const cliUpdateCheckInterval = 24 * time.Hour
const cliUpdateRetryInterval = time.Hour
const cliUpdateExitWait = time.Second

var pendingCLIUpdate struct {
	sync.Mutex
	done   chan struct{}
	cancel context.CancelFunc
}

// Wait for the request already started by this invocation. A short command
// gives the request a bounded chance to persist before main exits.
func waitCLIUpdateCheck(ctx context.Context) {
	pendingCLIUpdate.Lock()
	done, cancel := pendingCLIUpdate.done, pendingCLIUpdate.cancel
	pendingCLIUpdate.Unlock()
	if done == nil {
		return
	}
	timer := time.NewTimer(cliUpdateExitWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
		cancel()
	case <-timer.C:
		cancel()
	}
}

// The request runs alongside the command; completion waits at most one second.
// The variable lets root-command tests avoid a real network request.
var scheduleCLIUpdateCheck = func() {
	pendingCLIUpdate.Lock()
	defer pendingCLIUpdate.Unlock()
	if pendingCLIUpdate.done != nil {
		select {
		case <-pendingCLIUpdate.done:
		default:
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	done := make(chan struct{})
	pendingCLIUpdate.done, pendingCLIUpdate.cancel = done, cancel
	go func() { defer close(done); defer cancel(); checkCLIUpdateIfDue(ctx) }()
}

// checkCLIUpdateIfDue is shared by ordinary invocations and the long-lived
// MCP server. Read the current cache each time; an hourly MCP tick only
// contacts GitHub when the existing 24-hour interval has elapsed.
func checkCLIUpdateIfDue(ctx context.Context) {
	if ctx.Err() != nil || runsAsForeignUser() || backgroundCLIUpdatesDisabled() {
		return
	}
	cfg, err := config.Load()
	if err != nil || !dueCLIUpdateCheck(cfg) {
		return
	}
	// Record the attempt before I/O so a timeout or short-lived process does
	// not retry on every command. This does not claim a successful check.
	if err := updateCLIState(ctx, func(current *config.Config) {
		current.LastCLIUpdateAttempt = time.Now().UTC().Format(time.RFC3339)
	}); err != nil {
		return
	}
	latest, checkErr := checkLatestReleaseContext(ctx)
	if ctx.Err() != nil {
		return
	}
	_ = persistCLIUpdateCheckResultContext(ctx, time.Now(), latest, checkErr)
}

// persistCLIUpdateCheckResult persists one check's outcome by changing only its
// own fields on the CURRENT config. It used to save the snapshot root
// loaded at startup, seconds after the command had saved its own changes — a
// fresh pin, a login — and silently revert them. It also
// holds the auth refresh lock: the HTTP request can overlap an OAuth refresh,
// and saving a config read before that refresh finished would restore the
// consumed refresh token and cause wendy-auth to revoke the rotated token
// family on its next use.
func persistCLIUpdateCheckResult(checkedAt time.Time, latest string, checkErr error) error {
	return persistCLIUpdateCheckResultContext(context.Background(), checkedAt, latest, checkErr)
}

func persistCLIUpdateCheckResultContext(ctx context.Context, checkedAt time.Time, latest string, checkErr error) error {
	return updateCLIState(ctx, func(cfg *config.Config) {
		cfg.LastCLIUpdateAttempt = checkedAt.UTC().Format(time.RFC3339)
		if checkErr == nil {
			cfg.LastCLIUpdateCheck = cfg.LastCLIUpdateAttempt
			cfg.LatestCLIRelease = latest
			if version.CompareVersions(latest, version.Version) > 0 {
				cfg.AvailableCLIUpdate = latest
			} else {
				cfg.AvailableCLIUpdate = ""
			}
		}
	})
}

// dueCLIUpdateCheck returns true when the CLI is a released build and enough
// time has passed since the last check.
func dueCLIUpdateCheck(cfg *config.Config) bool {
	if version.IsDev(version.Version) {
		return false
	}
	if timestampWithin(cfg.LastCLIUpdateCheck, cliUpdateCheckInterval, time.Now()) {
		return false
	}
	return !timestampWithin(cfg.LastCLIUpdateAttempt, cliUpdateRetryInterval, time.Now())
}

func timestampWithin(value string, interval time.Duration, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, value)
	return err == nil && !t.After(now) && now.Sub(t) < interval
}

// Release names may appear in terminal instructions. Reject whitespace, shell
// syntax, development tags and malformed dates from the release response.
var cliReleaseTagPattern = regexp.MustCompile(`^v?(?:[0-9]{4}\.[0-9]{2}\.[0-9]{2}-[0-9]{6}|[0-9]+\.[0-9]+\.[0-9]+)$`)

func validCLIReleaseTag(tag string) bool {
	if len(tag) > 64 || !cliReleaseTagPattern.MatchString(tag) {
		return false
	}
	value := strings.TrimPrefix(tag, "v")
	if strings.Contains(value, "-") {
		_, err := time.Parse("2006.01.02-150405", value)
		return err == nil
	}
	return true
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&release); err != nil {
		return "", fmt.Errorf("decoding release: %w", err)
	}

	if !validCLIReleaseTag(release.TagName) {
		return "", fmt.Errorf("release service returned an invalid CLI version")
	}
	return release.TagName, nil
}
