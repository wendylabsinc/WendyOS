package supervisor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

const (
	fetchBuild = "build-1"
	newest     = "2026.11.05-081952"
)

// releaseServer serves a GitHub-like release API with one agent tarball per tag.
type releaseServer struct {
	*httptest.Server
	mu        sync.Mutex
	hits      map[string]int
	agent     []byte
	digest    func(archive []byte) string
	latestTag string
	// onDownload runs before the archive is served.
	onDownload func()
	truncated  bool
}

func newReleaseServer(t *testing.T) *releaseServer {
	t.Helper()
	rs := &releaseServer{hits: map[string]int{}, agent: []byte("\x7fELF a new agent"), latestTag: newest}
	rs.digest = func(archive []byte) string {
		sum := sha256.Sum256(archive)
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	rs.Server = httptest.NewServer(http.HandlerFunc(rs.serve))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *releaseServer) archive() []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range []struct {
		name string
		data []byte
	}{
		{"wendy-agent-linux-arm64/wendy-agent-supervisor", []byte("supervisor")},
		{"wendy-agent-linux-arm64/wendy-agent", rs.agent},
	} {
		tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.data)), Typeflag: tar.TypeReg})
		tw.Write(e.data)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func (rs *releaseServer) serve(w http.ResponseWriter, r *http.Request) {
	rs.mu.Lock()
	rs.hits[r.URL.Path]++
	rs.mu.Unlock()
	tag := ""
	switch {
	case r.URL.Path == "/repos/wendylabsinc/wendy-agent/releases/latest":
		tag = rs.latestTag
	case strings.HasPrefix(r.URL.Path, "/repos/wendylabsinc/wendy-agent/releases/tags/"):
		tag = strings.TrimPrefix(r.URL.Path, "/repos/wendylabsinc/wendy-agent/releases/tags/")
	case strings.HasPrefix(r.URL.Path, "/download/"):
		if rs.onDownload != nil {
			rs.onDownload()
		}
		w.Write(rs.archive())
		return
	default:
		http.NotFound(w, r)
		return
	}
	asset := map[string]any{
		"name":                 "wendy-agent-linux-arm64-" + tag + ".tar.gz",
		"browser_download_url": rs.URL + "/download/" + tag + "/arm64.tar.gz",
	}
	if d := rs.digest(rs.archive()); d != "" {
		asset["digest"] = d
	}
	if rs.truncated {
		w.Write([]byte(`{"tag_name": "` + tag + `", "assets": [`))
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"tag_name": tag,
		"assets": []any{
			map[string]any{"name": "wendy-agent-linux-amd64-" + tag + ".tar.gz", "browser_download_url": rs.URL + "/download/amd64", "digest": "sha256:00"},
			asset,
		},
	})
}

func (rs *releaseServer) downloads() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	n := 0
	for path, c := range rs.hits {
		if strings.HasPrefix(path, "/download/") {
			n += c
		}
	}
	return n
}

func (rs *releaseServer) requests() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	n := 0
	for _, c := range rs.hits {
		n += c
	}
	return n
}

type fetchFixture struct {
	f      *Fetcher
	rs     *releaseServer
	wakes  int
	status Status
}

func newFetchFixture(t *testing.T) *fetchFixture {
	t.Helper()
	dir := t.TempDir()
	fx := &fetchFixture{rs: newReleaseServer(t)}
	fx.status = Status{Role: RoleImage, PID: 42, ImageVersion: "2026.11.01-000000", AgentStarted: time.Minute}
	fx.f = &Fetcher{
		Store:         Store{Dir: filepath.Join(dir, "agent")},
		StatusPath:    filepath.Join(dir, "status.json"),
		Build:         fetchBuild,
		Arch:          "arm64",
		Repo:          "wendylabsinc/wendy-agent",
		API:           fx.rs.URL,
		Client:        fx.rs.Client(),
		RemoteSeen:    func(pid int, via string) bool { return pid == 42 && via == ViaCloud },
		CloudEnrolled: func(pid int) bool { return pid == 42 },
		Uptime:        func() time.Duration { return time.Hour },
		Settle:        3 * time.Minute,
		Wake: func(context.Context) error {
			fx.wakes++
			return nil
		},
		Log: zap.NewNop(),
	}
	return fx
}

func (fx *fetchFixture) run(t *testing.T, st State) (string, error) {
	t.Helper()
	if err := writeStatus(fx.f.StatusPath, fx.status); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fx.f.Store.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(fx.f.Store.StatePath(), st); err != nil {
		t.Fatal(err)
	}
	return fx.f.Fetch(context.Background())
}

func (fx *fetchFixture) staged(t *testing.T) []Incoming {
	t.Helper()
	metas, _ := filepath.Glob(filepath.Join(fx.f.Store.InboxDir(), "*.json"))
	var out []Incoming
	for _, m := range metas {
		var in Incoming
		data, err := os.ReadFile(m)
		if err != nil || json.Unmarshal(data, &in) != nil {
			t.Fatalf("unreadable %s", m)
		}
		out = append(out, in)
	}
	return out
}

func (fx *fetchFixture) inboxEntries(t *testing.T) []string {
	t.Helper()
	entries, _ := os.ReadDir(fx.f.Store.InboxDir())
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestFetchStagesANewerRelease(t *testing.T) {
	fx := newFetchFixture(t)
	msg, err := fx.run(t, State{})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(fx.rs.agent)
	want := Incoming{SHA256: hex.EncodeToString(sum[:]), RemoteVia: ViaCloud, Version: newest, Replaces: ReplacesImage, Build: fetchBuild}
	got := fx.staged(t)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("staged %+v, want %+v (%s)", got, want, msg)
	}
	data, err := os.ReadFile(filepath.Join(fx.f.Store.InboxDir(), want.SHA256))
	if err != nil || !bytes.Equal(data, fx.rs.agent) {
		t.Fatalf("the inbox must hold the release's agent, got %q, %v", data, err)
	}
	// The supervisor's check runs it before anything else.
	if fi, err := os.Stat(filepath.Join(fx.f.Store.InboxDir(), want.SHA256)); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("the staged agent must be executable, got %v, %v", fi.Mode(), err)
	}
	if fx.wakes != 1 {
		t.Fatalf("the supervisor must be woken once, got %d", fx.wakes)
	}
	if n := len(fx.inboxEntries(t)); n != 2 {
		t.Fatalf("only the agent and its metadata may be left in the inbox, got %v", fx.inboxEntries(t))
	}
}

func TestFetchReplacesTheApprovedAgent(t *testing.T) {
	fx := newFetchFixture(t)
	fx.status = Status{Role: RoleApproved, PID: 42, Version: "2026.11.02-000000", ImageVersion: "2026.11.01-000000"}
	if _, err := fx.run(t, State{Approved: &Agent{SHA256: "users", Version: "2026.11.02-000000", Builds: []string{fetchBuild}}}); err != nil {
		t.Fatal(err)
	}
	if got := fx.staged(t); len(got) != 1 || got[0].Replaces != "users" {
		t.Fatalf("the update must name the agent it replaces, got %+v", got)
	}
}

func TestFetchAsksForLocalChecksWithoutTheCloud(t *testing.T) {
	fx := newFetchFixture(t)
	fx.f.RemoteSeen = func(int, string) bool { return false }
	fx.f.CloudEnrolled = func(int) bool { return false }
	if _, err := fx.run(t, State{}); err != nil {
		t.Fatal(err)
	}
	if got := fx.staged(t); len(got) != 1 || got[0].RemoteVia != "" {
		t.Fatalf("a device that never reached the cloud cannot be asked to, got %+v", got)
	}
}

func TestFetchLeavesTheDeviceAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status Status
		state  State
		pin    string
		lookup bool // whether the release is looked up before skipping
	}{
		{name: "up to date", status: Status{Role: RoleImage, PID: 42, ImageVersion: newest}, lookup: true},
		{name: "newer than the release", status: Status{Role: RoleImage, PID: 42, ImageVersion: "2026.12.01-000000"}, lookup: true},
		{name: "a dev build", status: Status{Role: RoleImage, PID: 42, ImageVersion: "2026.11.01-000000-dev"}},
		{name: "a dev build even with a pin", status: Status{Role: RoleImage, PID: 42, ImageVersion: "dev"}, pin: newest},
		{name: "an agent that just started", status: Status{Role: RoleImage, PID: 42, ImageVersion: "2026.11.01-000000", AgentStarted: time.Hour - time.Minute}},
		{name: "a release that missed its confirm too often", status: Status{Role: RoleImage, PID: 42, ImageVersion: "2026.11.01-000000"}, state: State{
			Rejected: []Rejection{
				{SHA256: "x", Version: newest, Build: fetchBuild, Unconfirmed: true},
				{SHA256: "x", Version: newest, Build: fetchBuild, Unconfirmed: true},
				{SHA256: "x", Version: newest, Build: fetchBuild, Unconfirmed: true},
			},
		}, lookup: true},
		{name: "an agent on trial", status: Status{Role: RoleTrial, PID: 42, ImageVersion: "2026.11.01-000000"}},
		{name: "an agent in its grace", status: Status{Role: RoleApproved, Grace: true, PID: 42, ImageVersion: "2026.11.01-000000"}},
		{name: "updates paused", status: Status{Role: RoleImage, PID: 42, ImageVersion: "2026.11.01-000000", UpdatesPaused: "agent updates are off: /data is not mounted"}},
		{name: "a release that failed here", status: Status{Role: RoleImage, PID: 42, ImageVersion: "2026.11.01-000000"}, state: State{
			Rejected: []Rejection{{SHA256: "x", Version: newest, Build: fetchBuild, Reason: "crashed"}},
		}, lookup: true},
		{name: "already at the pinned release", status: Status{Role: RoleImage, PID: 42, ImageVersion: "2026.10.20-000000"}, pin: "2026.10.20-000000", lookup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFetchFixture(t)
			fx.status = tc.status
			fx.f.Pin = tc.pin
			msg, err := fx.run(t, tc.state)
			if err != nil {
				t.Fatal(err)
			}
			if got := fx.staged(t); len(got) != 0 || fx.wakes != 0 || fx.rs.downloads() != 0 {
				t.Fatalf("nothing may be fetched: staged %+v, wakes %d, downloads %d (%s)", got, fx.wakes, fx.rs.downloads(), msg)
			}
			if !tc.lookup && fx.rs.requests() != 0 {
				t.Fatalf("GitHub need not be asked: %s", msg)
			}
			if msg == "" {
				t.Fatal("the log must say why")
			}
		})
	}
}

func TestFetchStopsWaitingForACloudItNeverReaches(t *testing.T) {
	fx := newFetchFixture(t)
	fx.f.RemoteSeen = func(int, string) bool { return false }
	// Counted in runs, not uptime: a device switched off every night still gets there.
	for run := 1; run <= MaxCloudDeferrals; run++ {
		fx.status.PID = 42 + run
		fx.f.CloudEnrolled = func(int) bool { return true }
		if msg, err := fx.run(t, State{}); err != nil || len(fx.staged(t)) != 0 {
			t.Fatalf("run %d must wait: %q, %v", run, msg, err)
		}
	}
	if _, err := fx.run(t, State{}); err != nil {
		t.Fatal(err)
	}
	// Its relay never worked: holding updates back would also hold back a fix for it.
	if got := fx.staged(t); len(got) != 1 || got[0].RemoteVia != "" {
		t.Fatalf("staged %+v", got)
	}
}

func TestFetchForgetsTheWaitOnceTheCloudIsReached(t *testing.T) {
	fx := newFetchFixture(t)
	fx.f.RemoteSeen = func(int, string) bool { return false }
	for run := 0; run < MaxCloudDeferrals-1; run++ {
		fx.run(t, State{})
	}
	fx.f.RemoteSeen = func(pid int, via string) bool { return via == ViaCloud }
	fx.status.ImageVersion = newest // up to date: nothing to stage
	fx.run(t, State{})
	fx.f.RemoteSeen = func(int, string) bool { return false }
	fx.status.ImageVersion = "2026.11.01-000000"
	for run := 1; run <= MaxCloudDeferrals; run++ {
		if msg, _ := fx.run(t, State{}); len(fx.staged(t)) != 0 {
			t.Fatalf("a device that reached the cloud since waits afresh, staged on run %d: %q", run, msg)
		}
	}
}

func TestFetchWaitsForTheCloudWhenTheDeviceUsesIt(t *testing.T) {
	fx := newFetchFixture(t)
	fx.f.RemoteSeen = func(int, string) bool { return false }
	msg, err := fx.run(t, State{})
	if err != nil || len(fx.staged(t)) != 0 || fx.rs.downloads() != 0 {
		t.Fatalf("an enrolled device that is offline from the cloud must wait: %q, %v", msg, err)
	}
}

func TestFetchCountsOnlyRunsThatHeldAnUpdateBack(t *testing.T) {
	for name, setup := range map[string]func(fx *fetchFixture){
		"up to date": func(fx *fetchFixture) { fx.status.ImageVersion = newest },
		"offline":    func(fx *fetchFixture) { fx.rs.Close() },
	} {
		t.Run(name, func(t *testing.T) {
			fx := newFetchFixture(t)
			fx.f.RemoteSeen = func(int, string) bool { return false }
			setup(fx)
			fx.run(t, State{})
			if _, err := os.Stat(fx.f.Store.CloudDeferralsPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a run that had nothing to hold back must not count against the cloud")
			}
		})
	}
}

func TestFetchIgnoresAGarbageDeferralCount(t *testing.T) {
	fx := newFetchFixture(t)
	fx.f.RemoteSeen = func(int, string) bool { return false }
	os.MkdirAll(fx.f.Store.InboxDir(), 0o755)
	os.WriteFile(fx.f.Store.CloudDeferralsPath(), []byte("-7"), 0o644)
	fx.run(t, State{})
	if got, _ := os.ReadFile(fx.f.Store.CloudDeferralsPath()); string(got) != "1" {
		t.Fatalf("count = %q", got)
	}
}

func TestFetchRetriesAReleaseThatOnlyMissedItsConfirm(t *testing.T) {
	fx := newFetchFixture(t)
	sum := sha256.Sum256(fx.rs.agent)
	st := State{Rejected: []Rejection{{SHA256: hex.EncodeToString(sum[:]), Version: newest, Build: fetchBuild, Unconfirmed: true}}}
	if _, err := fx.run(t, st); err != nil {
		t.Fatal(err)
	}
	if got := fx.staged(t); len(got) != 1 {
		t.Fatal("a cloud blip during the trial must not block the release")
	}
}

func TestFetchTidiesTheInbox(t *testing.T) {
	fx := newFetchFixture(t)
	inbox := fx.f.Store.InboxDir()
	os.MkdirAll(inbox, 0o755)
	os.WriteFile(filepath.Join(inbox, fetchTempPrefix+"archive-1"), []byte("interrupted"), 0o600)
	stale := Incoming{SHA256: strings.Repeat("b", 64), Replaces: ReplacesImage, Build: "build-0", Version: "2026.11.03-000000"}
	os.WriteFile(filepath.Join(inbox, stale.SHA256), []byte("old agent"), 0o644)
	WriteIncoming(inbox, stale)
	if _, err := fx.run(t, State{}); err != nil {
		t.Fatal(err)
	}
	got := fx.staged(t)
	if len(got) != 1 || got[0].Version != newest {
		t.Fatalf("the stale update must make way for the new one, got %+v", got)
	}
	for _, name := range fx.inboxEntries(t) {
		if strings.HasPrefix(name, fetchTempPrefix) || strings.HasPrefix(name, stale.SHA256) {
			t.Fatalf("left %s behind", name)
		}
	}
}

func TestFetchWakesTheSupervisorForAWaitingUpdate(t *testing.T) {
	fx := newFetchFixture(t)
	inbox := fx.f.Store.InboxDir()
	os.MkdirAll(inbox, 0o755)
	waiting := Incoming{SHA256: strings.Repeat("c", 64), Replaces: ReplacesImage, Build: fetchBuild, Version: newest}
	os.WriteFile(filepath.Join(inbox, waiting.SHA256), []byte("agent"), 0o644)
	WriteIncoming(inbox, waiting)
	msg, err := fx.run(t, State{})
	if err != nil || fx.wakes != 1 || fx.rs.requests() != 0 || len(fx.staged(t)) != 1 {
		t.Fatalf("a waiting update gets another wake, not a second download: %q, %v, wakes %d", msg, err, fx.wakes)
	}
}

func TestFetchDropsADownloadWhenATrialStarts(t *testing.T) {
	fx := newFetchFixture(t)
	fx.rs.onDownload = func() {
		writeStatus(fx.f.StatusPath, Status{Role: RoleTrial, PID: 77, ImageVersion: "2026.11.01-000000"})
	}
	msg, err := fx.run(t, State{})
	if err != nil || len(fx.staged(t)) != 0 || fx.wakes != 0 {
		t.Fatalf("the user's trial came first: %q, %v", msg, err)
	}
	if n := len(fx.inboxEntries(t)); n != 0 {
		t.Fatalf("the download must be removed, left %v", fx.inboxEntries(t))
	}
}

func TestFetchTreatsATruncatedAnswerAsOffline(t *testing.T) {
	fx := newFetchFixture(t)
	fx.rs.truncated = true
	if msg, err := fx.run(t, State{}); err != nil || msg == "" {
		t.Fatalf("a cut-off answer is retried tomorrow: %q, %v", msg, err)
	}
}

func TestReadOnlyInboxIsRetried(t *testing.T) {
	if !readOnly(&os.PathError{Op: "open", Path: "/data/wendy-agent/agent/inbox/x", Err: syscall.EROFS}) {
		t.Fatal("a read-only inbox (the updater started before it existed) must be retried")
	}
	if readOnly(errors.New("disk full")) {
		t.Fatal("other errors are failures")
	}
}

func TestFetchHoldsTheDeviceOnThePinnedRelease(t *testing.T) {
	fx := newFetchFixture(t)
	fx.f.Pin = "2026.10.20-000000"
	if _, err := fx.run(t, State{}); err != nil {
		t.Fatal(err)
	}
	// Older than the image's agent: a pin is honoured exactly, as the shell updater did.
	if got := fx.staged(t); len(got) != 1 || got[0].Version != "2026.10.20-000000" {
		t.Fatalf("staged %+v", got)
	}
}

func TestFetchSkipsAnAgentItAlreadyHas(t *testing.T) {
	fx := newFetchFixture(t)
	sum := sha256.Sum256(fx.rs.agent)
	sha := hex.EncodeToString(sum[:])
	fx.status = Status{Role: RoleImage, PID: 42, ImageVersion: "2026.11.01-000000"}
	// The release's agent failed here before, under a version the rejection did not record.
	if _, err := fx.run(t, State{Rejected: []Rejection{{SHA256: sha, Build: fetchBuild}}}); err != nil {
		t.Fatal(err)
	}
	if got := fx.staged(t); len(got) != 0 || fx.wakes != 0 {
		t.Fatalf("a rejected agent must not be staged again: %+v", got)
	}
	if n := len(fx.inboxEntries(t)); n != 0 {
		t.Fatalf("the download must be cleaned up, got %v", fx.inboxEntries(t))
	}
}

func TestFetchRefusesAnUnverifiedRelease(t *testing.T) {
	for name, digest := range map[string]func([]byte) string{
		"a digest mismatch": func([]byte) string { return "sha256:" + strings.Repeat("0", 64) },
		"no digest":         func([]byte) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			fx := newFetchFixture(t)
			fx.rs.digest = digest
			if _, err := fx.run(t, State{}); err == nil {
				t.Fatal("an unverified release must fail the run")
			}
			if n := len(fx.inboxEntries(t)); n != 0 || fx.wakes != 0 {
				t.Fatalf("nothing may be left in the inbox, got %v", fx.inboxEntries(t))
			}
		})
	}
}

func TestFetchOfflineIsNotAFailure(t *testing.T) {
	fx := newFetchFixture(t)
	fx.rs.Close()
	msg, err := fx.run(t, State{})
	if err != nil || msg == "" {
		t.Fatalf("an offline device just tries again tomorrow: %q, %v", msg, err)
	}
}

func TestFetchWithoutTheSupervisor(t *testing.T) {
	fx := newFetchFixture(t)
	msg, err := fx.f.Fetch(context.Background())
	if err != nil || fx.rs.requests() != 0 || msg == "" {
		t.Fatalf("no supervisor status means no fetch: %q, %v", msg, err)
	}
}

func TestFetchSettings(t *testing.T) {
	dir := t.TempDir()
	etc, data := filepath.Join(dir, "etc"), filepath.Join(dir, "data")
	os.WriteFile(etc, []byte("# operator settings\nWENDYOS_AGENT_GITHUB_REPO=acme/agent\nWENDYOS_AGENT_VERSION=\"2026.10.01-000000\"\n"), 0o644)
	os.WriteFile(data, []byte("export WENDYOS_AGENT_VERSION='2026.10.20-000000' # held for a regression\n"), 0o644)
	env := func(k string) string {
		if k == "WENDYOS_AGENT_VERSION" {
			return "2026.09.01-000000"
		}
		return ""
	}
	repo, pin := FetchSettings(env, etc, data, filepath.Join(dir, "missing"))
	if repo != "acme/agent" || pin != "2026.10.20-000000" {
		t.Fatalf("the /data copy wins, like the shell updater: repo %q, pin %q", repo, pin)
	}
	repo, pin = FetchSettings(func(string) string { return "" }, filepath.Join(dir, "missing"))
	if repo != DefaultAgentRepo || pin != "" {
		t.Fatalf("defaults: repo %q, pin %q", repo, pin)
	}
	os.WriteFile(data, []byte("WENDYOS_AGENT_VERSION=latest # follow stable\n"), 0o644)
	if _, pin = FetchSettings(env, etc, data); pin != "" {
		t.Fatalf("latest means no pin, got %q", pin)
	}
	os.WriteFile(data, []byte("WENDYOS_AGENT_VERSION= # unpinned for now\n"), 0o644)
	if _, pin = FetchSettings(env, etc, data); pin != "" {
		t.Fatalf("an empty value with a comment is empty, got %q", pin)
	}
}
