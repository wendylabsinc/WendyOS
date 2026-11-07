package supervisor

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"go.uber.org/zap"
)

// DefaultAgentRepo publishes the agent releases.
const DefaultAgentRepo = "wendylabsinc/WendyOS"

// maxDownload bounds what a release may make the device write to /data.
const maxDownload = 512 << 20

// MaxCloudDeferrals is how many fetch runs in a row an agent that is enrolled
// with the cloud but not connected may hold an update back. Past it the relay
// is broken for this device, and a fix for that must still get through.
const MaxCloudDeferrals = 4

// fetchTempPrefix names a fetch's partial downloads in the inbox, which the
// supervisor never takes: it reads only <sha256>.json.
const fetchTempPrefix = ".fetch-"

// Fetcher is the automatic agent update on supervisor images: it puts a newer
// release's agent into the inbox, where the supervisor tries it like any upload.
type Fetcher struct {
	Store      Store
	StatusPath string
	Build      string
	Arch       string
	Repo       string
	// Pin holds the device on one release tag; "" follows the newest stable one.
	Pin    string
	API    string
	Client *http.Client
	// RemoteSeen reports whether the running agent reached the cloud, which
	// the new one must then do too; CloudEnrolled whether it means to.
	RemoteSeen    func(pid int, via string) bool
	CloudEnrolled func(pid int) bool
	// Uptime is the boot clock; Settle is how long the agent must have run
	// before its cloud connection says anything.
	Uptime func() time.Duration
	Settle time.Duration
	// Wake tells the supervisor an update is waiting.
	Wake func(context.Context) error
	Log  *zap.Logger
}

type ghRelease struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

// Fetch returns what it did for the log. Being offline is not an error: the
// timer tries again. A release that fails verification is.
func (f *Fetcher) Fetch(ctx context.Context) (string, error) {
	status, err := ReadStatus(f.StatusPath)
	if err != nil {
		return "the agent supervisor is not running; nothing to update", nil
	}
	switch {
	case status.UpdatesPaused != "":
		return status.UpdatesPaused, nil
	case status.Role == RoleTrial, status.Grace:
		return "an agent update is being verified; trying again later", nil
	case status.PID == 0:
		return "the agent is not running; trying again later", nil
	}
	age := f.Uptime() - status.AgentStarted
	if age < f.Settle {
		return fmt.Sprintf("the agent started %s ago; trying again later", age.Round(time.Second)), nil
	}
	st, err := LoadState(f.Store.StatePath())
	if err != nil {
		return "", err
	}
	if msg, err := f.tidyInbox(ctx, st); msg != "" || err != nil {
		return msg, err
	}
	reached, enrolled := f.RemoteSeen(status.PID, ViaCloud), f.CloudEnrolled(status.PID)
	if reached || !enrolled {
		os.Remove(f.Store.CloudDeferralsPath())
	}
	replaces, current := st.Replaceable(f.Build), status.ImageVersion
	if replaces != ReplacesImage {
		current = st.Approved.Version
	}
	// Someone put this agent here on purpose.
	switch {
	case current == "":
		return "the running agent's version is unknown; not updating it", nil
	case version.IsDev(current):
		return fmt.Sprintf("agent %s is a dev build; not updating it", current), nil
	case f.Pin != "" && strings.TrimPrefix(f.Pin, "v") == current:
		return fmt.Sprintf("already at the pinned agent %s", current), nil
	}

	rel, skip, err := f.release(ctx)
	if skip != "" || err != nil {
		return skip, err
	}
	tag := strings.TrimPrefix(rel.Tag, "v")
	switch {
	case st.GivenUp("", tag, f.Build):
		return fmt.Sprintf("agent %s already failed its checks on this OS build", tag), nil
	case f.Pin == "" && version.CompareVersions(tag, current) <= 0:
		return fmt.Sprintf("agent %s is up to date (newest release %s)", current, tag), nil
	}
	// Counted only when there is an update and GitHub answered: the relay, not
	// the network, is what fails.
	if !reached && enrolled {
		if n := f.cloudDeferrals(); n < MaxCloudDeferrals {
			f.countCloudDeferral(n + 1)
			return fmt.Sprintf("agent %s waits until the agent is connected to Wendy Cloud (%d of %d)", tag, n+1, MaxCloudDeferrals), nil
		}
	}

	sha, skip, err := f.download(ctx, rel)
	if skip != "" || err != nil {
		return skip, err
	}
	inbox := f.Store.InboxDir()
	fetched := filepath.Join(inbox, fetchTempPrefix+sha)
	switch {
	case st.GivenUp(sha, "", f.Build):
		os.Remove(fetched)
		return fmt.Sprintf("agent %s already failed its checks on this OS build", tag), nil
	case sha == replaces:
		os.Remove(fetched)
		return fmt.Sprintf("agent %s is already installed", tag), nil
	}
	if why := f.changedSince(ctx, status); why != "" {
		os.Remove(fetched)
		return why + "; trying again later", nil
	}
	if err := os.Rename(fetched, filepath.Join(inbox, sha)); err != nil {
		os.Remove(fetched)
		return "", err
	}
	via := ""
	if reached {
		via = ViaCloud
	}
	in := Incoming{SHA256: sha, RemoteVia: via, Version: tag, Replaces: replaces, Build: f.Build}
	if err := WriteIncoming(inbox, in); err != nil {
		os.Remove(filepath.Join(inbox, sha))
		return "", err
	}
	msg := fmt.Sprintf("agent %s is waiting for its trial (replacing %s)", tag, current)
	if err := f.Wake(ctx); err != nil {
		// It still runs at the agent's next start.
		f.Log.Warn("could not tell the agent supervisor", zap.Error(err))
		msg += "; it starts with the agent's next restart"
	}
	return msg, nil
}

func (f *Fetcher) cloudDeferrals() int {
	data, _ := os.ReadFile(f.Store.CloudDeferralsPath())
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return max(n, 0)
}

func (f *Fetcher) countCloudDeferral(n int) {
	err := os.MkdirAll(f.Store.InboxDir(), 0o755)
	if err == nil {
		err = atomicfile.Write(f.Store.CloudDeferralsPath(), []byte(strconv.Itoa(n)), 0o644)
	}
	if err != nil {
		f.Log.Warn("could not count the wait for the cloud", zap.Error(err))
	}
}

// changedSince says why the device is no longer as the fetch found it: an
// upload or a trial came first, or the run is being stopped.
func (f *Fetcher) changedSince(ctx context.Context, before Status) string {
	if ctx.Err() != nil {
		return "stopped"
	}
	now, err := ReadStatus(f.StatusPath)
	switch {
	case err != nil, now.PID != before.PID, now.Role != before.Role, now.UpdatesPaused != "":
		return "the agent changed during the download"
	}
	if metas, _ := filepath.Glob(filepath.Join(f.Store.InboxDir(), "*.json")); len(metas) > 0 {
		return "another agent update arrived during the download"
	}
	return ""
}

// tidyInbox removes what an interrupted fetch or a stale update left on
// /data, and wakes the supervisor again for an update that is still waiting.
func (f *Fetcher) tidyInbox(ctx context.Context, st State) (string, error) {
	inbox := f.Store.InboxDir()
	old, _ := filepath.Glob(filepath.Join(inbox, fetchTempPrefix+"*"))
	for _, p := range old {
		os.Remove(p)
	}
	metas, _ := filepath.Glob(filepath.Join(inbox, "*.json"))
	waiting := false
	for _, m := range metas {
		var in Incoming
		data, err := os.ReadFile(m)
		if err == nil {
			err = json.Unmarshal(data, &in)
		}
		if err == nil && staleAutomatic(in, st, f.Build) {
			os.Remove(m)
			os.Remove(strings.TrimSuffix(m, ".json"))
			continue
		}
		waiting = true
	}
	if !waiting {
		return "", nil
	}
	if err := f.Wake(ctx); err != nil {
		f.Log.Warn("could not tell the agent supervisor", zap.Error(err))
	}
	return "an agent update is already waiting for the supervisor", nil
}

// readOnly is the inbox before the updater's sandbox could bind it writable:
// the unit started before the supervisor created it.
func readOnly(err error) bool {
	return errors.Is(err, syscall.EROFS)
}

// release looks up the pinned or the newest stable release.
func (f *Fetcher) release(ctx context.Context) (ghRelease, string, error) {
	u := fmt.Sprintf("%s/repos/%s/releases/latest", f.API, f.Repo)
	if f.Pin != "" {
		u = fmt.Sprintf("%s/repos/%s/releases/tags/%s", f.API, f.Repo, url.PathEscape(f.Pin))
	}
	var rel ghRelease
	resp, skip, err := f.get(ctx, u, "application/vnd.github+json")
	if skip != "" || err != nil {
		return rel, skip, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return rel, fmt.Sprintf("could not read the release from %s: %v", u, err), nil
	}
	if rel.Tag == "" {
		return rel, "", fmt.Errorf("release %s has no tag", u)
	}
	return rel, "", nil
}

// download writes the release's agent to the inbox as .fetch-<sha256> and
// returns its sha256. The archive must match the digest GitHub records for it.
func (f *Fetcher) download(ctx context.Context, rel ghRelease) (string, string, error) {
	prefix := "wendy-agent-linux-" + f.Arch + "-"
	var assetURL, digest string
	for _, a := range rel.Assets {
		if strings.HasPrefix(a.Name, prefix) && strings.HasSuffix(a.Name, ".tar.gz") {
			assetURL, digest = a.URL, a.Digest
			break
		}
	}
	if assetURL == "" {
		return "", "", fmt.Errorf("release %s has no agent for %s", rel.Tag, f.Arch)
	}
	want, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(want) != sha256.Size*2 {
		return "", "", fmt.Errorf("release %s publishes no sha256 for its agent; not installing it", rel.Tag)
	}

	inbox := f.Store.InboxDir()
	err := os.MkdirAll(inbox, 0o755)
	var archive *os.File
	if err == nil {
		archive, err = os.CreateTemp(inbox, fetchTempPrefix+"archive-")
	}
	if readOnly(err) {
		return "", "the agent store is not writable yet; trying again later", nil
	}
	if err != nil {
		return "", "", err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()

	resp, skip, err := f.get(ctx, assetURL, "application/octet-stream")
	if skip != "" || err != nil {
		return "", skip, err
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(archive, h), io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return "", fmt.Sprintf("downloading agent %s was interrupted: %v", rel.Tag, err), nil
	}
	if n > maxDownload {
		return "", "", fmt.Errorf("release %s: the agent archive is larger than %d bytes", rel.Tag, maxDownload)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return "", "", fmt.Errorf("release %s: the agent archive's sha256 is %s, GitHub records %s", rel.Tag, got, want)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return "", "", err
	}
	sha, err := extractAgent(bufio.NewReader(archive), inbox)
	if err != nil {
		return "", "", fmt.Errorf("release %s: %w", rel.Tag, err)
	}
	return sha, "", nil
}

// extractAgent copies the archive's wendy-agent to <dir>/.fetch-<sha256>.
func extractAgent(r io.Reader, dir string) (string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("the archive has no wendy-agent")
		}
		if err != nil {
			return "", err
		}
		if hdr.Typeflag != tar.TypeReg || path.Base(hdr.Name) != "wendy-agent" {
			continue
		}
		out, err := os.CreateTemp(dir, fetchTempPrefix+"agent-")
		if err != nil {
			return "", err
		}
		h := sha256.New()
		sha, err := copyAgent(out, h, tr)
		if err == nil {
			// CreateTemp makes it 0600; the supervisor's check must be able to run it.
			err = os.Chmod(out.Name(), 0o755)
		}
		if err != nil {
			os.Remove(out.Name())
			return "", err
		}
		if err := os.Rename(out.Name(), filepath.Join(dir, fetchTempPrefix+sha)); err != nil {
			os.Remove(out.Name())
			return "", err
		}
		return sha, nil
	}
}

func copyAgent(out *os.File, h hash.Hash, r io.Reader) (string, error) {
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(r, maxDownload+1))
	if err == nil && n > maxDownload {
		err = fmt.Errorf("the agent is larger than %d bytes", maxDownload)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return hex.EncodeToString(h.Sum(nil)), err
}

// get treats an unreachable or busy GitHub as a reason to wait, not a failure.
func (f *Fetcher) get(ctx context.Context, u, accept string) (*http.Response, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "wendy-agent-supervisor/"+version.Version)
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, fmt.Sprintf("cannot reach %s: %v", req.URL.Host, err), nil
	}
	if resp.StatusCode == http.StatusOK {
		return resp, "", nil
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", fmt.Errorf("%s: not found", u)
	}
	return nil, fmt.Sprintf("%s answered %s", req.URL.Host, resp.Status), nil
}

// FetchSettings reads the repo and pin like the shell updater did: the
// environment, then each file in order, the last one winning. The files are
// parsed, never run.
func FetchSettings(env func(string) string, files ...string) (repo, pin string) {
	vals := map[string]string{
		"WENDYOS_AGENT_GITHUB_REPO": env("WENDYOS_AGENT_GITHUB_REPO"),
		"WENDYOS_AGENT_VERSION":     env("WENDYOS_AGENT_VERSION"),
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimPrefix(strings.TrimSpace(line), "export ")
			k, v, ok := strings.Cut(line, "=")
			k = strings.TrimSpace(k)
			if _, known := vals[k]; !ok || !known {
				continue
			}
			v = strings.TrimSpace(v)
			if strings.HasPrefix(v, "#") {
				v = ""
			} else if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
				if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
					v = v[1 : 1+end]
				}
			} else if i := strings.IndexAny(v, " \t"); i >= 0 {
				// As in the shell: an unquoted value ends at a blank, where a comment may follow.
				v = v[:i]
			}
			vals[k] = v
		}
	}
	repo, pin = vals["WENDYOS_AGENT_GITHUB_REPO"], vals["WENDYOS_AGENT_VERSION"]
	if repo == "" {
		repo = DefaultAgentRepo
	}
	if pin == "latest" {
		pin = ""
	}
	return repo, pin
}
