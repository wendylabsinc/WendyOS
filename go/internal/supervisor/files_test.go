//go:build linux

package supervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// agentVariant writes a runnable copy of the test binary with a unique tail,
// so each variant has its own sha256.
func agentVariant(t *testing.T, dir, tag string) (path, sum string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("variant:"+tag)...)
	h := sha256.Sum256(data)
	sum = hex.EncodeToString(h[:])
	path = filepath.Join(dir, sum)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return path, sum
}

func dropInInbox(t *testing.T, s Store, tag string, meta Incoming) string {
	t.Helper()
	_, sum := agentVariant(t, s.InboxDir(), tag)
	meta.SHA256 = sum
	data, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(s.InboxDir(), sum+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return sum
}

func newInboxSupervisor(t *testing.T, run VersionRunner) *Supervisor {
	t.Helper()
	return &Supervisor{cfg: Config{
		Store:        Store{Dir: t.TempDir()},
		Arch:         runtime.GOARCH,
		ImageVersion: "2026.11.05-114424",
	}, deps: Deps{RunVersion: run, Log: zap.NewNop()}}
}

func TestAdoptInboxStartsATrial(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.02-101010"))
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{RemoteVia: "relay"})

	var st State
	reason, commit := s.adoptInbox(context.Background(), &st, "build-1")
	if reason != "" {
		t.Fatalf("unexpected rejection: %s", reason)
	}
	commit()
	if st.Trial == nil || st.Trial.SHA256 != sum || st.Trial.RemoteVia != "relay" || st.Trial.Version != "2026.11.02-101010" {
		t.Fatalf("want a trial of the new agent, got %+v", st.Trial)
	}
	if _, err := os.Stat(s.cfg.Store.BinPath(sum)); err != nil {
		t.Fatalf("the binary must move to bin/: %v", err)
	}
	if entries, _ := os.ReadDir(s.cfg.Store.InboxDir()); len(entries) != 0 {
		t.Fatalf("the inbox must be empty, has %d entries", len(entries))
	}
}

// runsOnlyExecutables fails like systemd-run does on a file without an x bit.
func runsOnlyExecutables(v string) VersionRunner {
	return func(_ context.Context, path string) (string, error) {
		if fi, err := os.Stat(path); err != nil || fi.Mode()&0o111 == 0 {
			return "", errors.New("permission denied")
		}
		return v + "\n", nil
	}
}

func TestAdoptInboxRunsAnUploadStagedWithoutTheExecBit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, runsOnlyExecutables("2026.11.02-101010"))
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Version: "2026.11.02-101010", Replaces: ReplacesImage, Build: "build-1"})
	if err := os.Chmod(filepath.Join(s.cfg.Store.InboxDir(), sum), 0o600); err != nil {
		t.Fatal(err)
	}
	var st State
	if reason, commit := s.adoptInbox(context.Background(), &st, "build-1"); reason != "" {
		t.Fatalf("a verified upload must not fail on its file mode: %s", reason)
	} else {
		commit()
	}
	if st.Trial == nil || st.Trial.SHA256 != sum {
		t.Fatalf("want a trial, got %+v", st.Trial)
	}
}

func TestAdoptInboxRejectsAFailedCheck(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, func(context.Context, string) (string, error) { return "", errors.New("exit status 127") })
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{})

	var st State
	reason, commit := s.adoptInbox(context.Background(), &st, "build-1")
	commit()
	if reason == "" || st.Trial != nil {
		t.Fatalf("want a rejection, got reason %q trial %+v", reason, st.Trial)
	}
	if !st.IsRejected(sum, "build-1") {
		t.Fatal("the failed agent must be recorded as rejected")
	}
	if entries, _ := os.ReadDir(s.cfg.Store.InboxDir()); len(entries) != 0 {
		t.Fatal("a rejected upload must be removed from the inbox")
	}
}

func TestAdoptInboxSkipsAnAutomaticRetryOfARejectedAgent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.02-101010"))
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{})
	st := State{Rejected: []Rejection{{SHA256: sum, Build: "build-1", Reason: "crashed"}}}
	reason, commit := s.adoptInbox(context.Background(), &st, "build-1")
	commit()
	if reason == "" || st.Trial != nil {
		t.Fatalf("an automatic retry must be skipped, got %q %+v", reason, st.Trial)
	}

	sum = dropInInbox(t, s.cfg.Store, "b", Incoming{Explicit: true})
	st.Rejected = append(st.Rejected, Rejection{SHA256: sum, Build: "build-1"})
	if reason, _ := s.adoptInbox(context.Background(), &st, "build-1"); reason != "" || st.Trial == nil {
		t.Fatalf("an explicit push must retry, got %q %+v", reason, st.Trial)
	}
}

func TestAdoptInboxWaitsWhileATrialRuns(t *testing.T) {
	s := newInboxSupervisor(t, printsVersion("2026.11.02-101010"))
	dropInInbox(t, s.cfg.Store, "a", Incoming{})
	st := State{Trial: trial("running-sha")}
	_, commit := s.adoptInbox(context.Background(), &st, "build-1")
	commit()
	if st.Trial.SHA256 != "running-sha" {
		t.Fatal("a running trial must not be replaced")
	}
	if entries, _ := os.ReadDir(s.cfg.Store.InboxDir()); len(entries) != 2 {
		t.Fatal("the upload must wait in the inbox")
	}
}

func TestSnapshotAndRestore(t *testing.T) {
	src := filepath.Join(t.TempDir(), "wendy-agent")
	if err := os.MkdirAll(filepath.Join(src, "cloud-relay"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(rel, data string) {
		if err := os.WriteFile(filepath.Join(src, rel), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("provisioning.json", "old")
	write("cloud-relay/keys.json", "old-keys")
	snap := filepath.Join(t.TempDir(), "snapshot")

	if err := snapshot(snap, []SnapshotDir{{Path: src}}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	write("provisioning.json", "changed by the trial")
	write("new-file", "x")
	if err := restore(snap, []SnapshotDir{{Path: src}}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(src, "provisioning.json"))
	keys, _ := os.ReadFile(filepath.Join(src, "cloud-relay", "keys.json"))
	if string(got) != "old" || string(keys) != "old-keys" {
		t.Fatalf("restore did not bring back the old files: %q %q", got, keys)
	}
	if _, err := os.Stat(filepath.Join(src, "new-file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("files added during the trial must be removed")
	}
	if fi, _ := os.Stat(filepath.Join(src, "provisioning.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("modes must survive, got %v", fi.Mode().Perm())
	}
}

func TestSnapshotSkipsAMissingDirectory(t *testing.T) {
	snap := filepath.Join(t.TempDir(), "snapshot")
	missing := filepath.Join(t.TempDir(), "absent")
	if err := snapshot(snap, []SnapshotDir{{Path: missing}}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := restore(snap, []SnapshotDir{{Path: missing}}); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

func TestSnapshotLeavesNothingWhenItFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any file")
	}
	src := filepath.Join(t.TempDir(), "wendy-agent")
	os.MkdirAll(src, 0o700)
	os.WriteFile(filepath.Join(src, "a"), []byte("a"), 0o600)
	os.WriteFile(filepath.Join(src, "unreadable"), []byte("b"), 0o000)
	snap := filepath.Join(t.TempDir(), "snapshot")
	if err := snapshot(snap, []SnapshotDir{{Path: src}}); err == nil {
		t.Fatal("want an error for an unreadable file")
	}
	// A half-written snapshot must never look complete: restoring it would
	// delete the files it is missing.
	if _, err := os.Stat(snap); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed snapshot must not exist, stat err = %v", err)
	}
}

func TestAdoptInboxKeepsTheUploadUntilTheStateIsSaved(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.02-101010"))
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Explicit: true})

	var st State
	if reason, _ := s.adoptInbox(context.Background(), &st, "build-1"); reason != "" {
		t.Fatal(reason)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.Store.InboxDir(), sum+".json")); err != nil {
		t.Fatal("the upload must stay in the inbox until the trial is saved")
	}

	// The save failed: the next pass finds the binary already moved to bin/.
	var again State
	if reason, _ := s.adoptInbox(context.Background(), &again, "build-1"); reason != "" || again.Trial == nil || again.Trial.SHA256 != sum {
		t.Fatalf("want the same trial again, got %q %+v", reason, again.Trial)
	}
}

func TestAdoptInboxLetsAnExplicitUploadReplaceARevalidation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.02-101010"))
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Explicit: true, RemoteVia: "lan"})
	st := State{Approved: approved("2026.10.30-154553", "old-build")}
	st.Trial = &Trial{Agent: *st.Approved, Revalidation: true}
	if reason, _ := s.adoptInbox(context.Background(), &st, "build-1"); reason != "" || st.Trial == nil || st.Trial.SHA256 != sum {
		t.Fatalf("the upload must replace the revalidation, got %q %+v", reason, st.Trial)
	}
}

func TestAdoptInboxRetriesTheApprovedAgentAfterAFailedRevalidation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.02-101010"))
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Explicit: true, RemoteVia: "lan"})
	st := State{
		Approved: &Agent{SHA256: sum, Version: "2026.11.02-101010", Builds: []string{"old-build"}},
		Rejected: []Rejection{{SHA256: sum, Build: "build-1", Reason: "failed re-validation"}},
	}
	if reason, _ := s.adoptInbox(context.Background(), &st, "build-1"); reason != "" || st.Trial == nil || st.Trial.SHA256 != sum {
		t.Fatalf("an explicit push of the approved agent must retry it, got %q %+v", reason, st.Trial)
	}
}

func TestAdoptInboxIgnoresAnAutomaticUploadOfTheAgentOnTrial(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.02-101010"))
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{})
	st := State{Trial: &Trial{Agent: Agent{SHA256: sum}, Revalidation: true}}
	reason, commit := s.adoptInbox(context.Background(), &st, "build-1")
	commit()
	if reason != "" || st.Trial.SHA256 != sum || !st.Trial.Revalidation {
		t.Fatalf("the running trial must continue, got %q %+v", reason, st.Trial)
	}
	if entries, _ := os.ReadDir(s.cfg.Store.InboxDir()); len(entries) != 0 {
		t.Fatal("the duplicate upload must be dropped")
	}
}

func TestAdoptInboxClearsOldFailuresOnAnExplicitRetry(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.02-101010"))
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Explicit: true})
	st := State{Failures: []Failure{{SHA256: sum, BootID: "boot-1", Count: TrialFailureLimit}}}
	if reason, _ := s.adoptInbox(context.Background(), &st, "build-1"); reason != "" {
		t.Fatal(reason)
	}
	if st.FailureCount(sum, "boot-1") != 0 {
		t.Fatal("an explicit retry starts with a fresh crash budget")
	}
}

func TestRestoreUndoesATypeChange(t *testing.T) {
	src := filepath.Join(t.TempDir(), "wendy-agent")
	os.MkdirAll(filepath.Join(src, "certs"), 0o700)
	os.WriteFile(filepath.Join(src, "certs", "device.pem"), []byte("old cert"), 0o600)
	os.WriteFile(filepath.Join(src, "key.pem"), []byte("old key"), 0o600)
	snap := filepath.Join(t.TempDir(), "snapshot")
	if err := snapshot(snap, []SnapshotDir{{Path: src}}); err != nil {
		t.Fatal(err)
	}
	// The trial turned a directory into a file and a file into a directory.
	os.RemoveAll(filepath.Join(src, "certs"))
	os.WriteFile(filepath.Join(src, "certs"), []byte("new layout"), 0o600)
	os.Remove(filepath.Join(src, "key.pem"))
	os.MkdirAll(filepath.Join(src, "key.pem"), 0o700)

	if err := restore(snap, []SnapshotDir{{Path: src}}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	cert, _ := os.ReadFile(filepath.Join(src, "certs", "device.pem"))
	key, _ := os.ReadFile(filepath.Join(src, "key.pem"))
	if string(cert) != "old cert" || string(key) != "old key" {
		t.Fatalf("restore left %q / %q", cert, key)
	}
}

func TestAdoptInboxGivesARePushedApprovedAgentAFreshBudget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.02-101010"))
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Explicit: true})
	st := State{
		Approved: &Agent{SHA256: sum, Version: "2026.11.02-101010", Builds: []string{"build-1"}},
		Failures: []Failure{{SHA256: sum, BootID: "boot-1", Count: ApprovedFailureLimit}},
	}
	s.adoptInbox(context.Background(), &st, "build-1")
	if st.FailureCount(sum, "boot-1") != 0 {
		t.Fatal("a user's re-push of the approved agent must give it another chance")
	}
}

func TestAdoptInboxKeepsTheUploadWhenStoppedDuringTheCheck(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := newInboxSupervisor(t, func(ctx context.Context, _ string) (string, error) {
		cancel()
		<-ctx.Done()
		return "", errors.New("signal: terminated")
	})
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Explicit: true})
	var st State
	reason, commit := s.adoptInbox(ctx, &st, "build-1")
	commit()
	if reason != "" || st.IsRejected(sum, "build-1") {
		t.Fatalf("a stop is not a verdict, got %q", reason)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.Store.InboxDir(), sum+".json")); err != nil {
		t.Fatal("the upload must wait for the next start")
	}
}

func TestRestorePutsBackASymlinkTheTrialReplaced(t *testing.T) {
	src := filepath.Join(t.TempDir(), "wendy-agent")
	os.MkdirAll(src, 0o700)
	os.WriteFile(filepath.Join(src, "target"), []byte("t"), 0o600)
	os.Symlink("target", filepath.Join(src, "link"))
	snap := filepath.Join(t.TempDir(), "snapshot")
	if err := snapshot(snap, []SnapshotDir{{Path: src}}); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(src, "link"))
	os.WriteFile(filepath.Join(src, "link"), []byte("a plain file now"), 0o600)
	if err := restore(snap, []SnapshotDir{{Path: src}}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got, err := os.Readlink(filepath.Join(src, "link")); err != nil || got != "target" {
		t.Fatalf("want the symlink back, got %q, %v", got, err)
	}
}

func TestAdoptInboxTurnsARevalidationIntoAUserTrialWhenTheUserSendsIt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.02-101010"))
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Explicit: true, RemoteVia: "lan"})
	st := State{
		Trial:    &Trial{Agent: Agent{SHA256: sum}, Revalidation: true, Boots: []string{"boot-1"}},
		Failures: []Failure{{SHA256: sum, BootID: "boot-1", Count: 2}},
	}
	s.adoptInbox(context.Background(), &st, "build-1")
	if st.Trial.Revalidation || st.Trial.RemoteVia != "lan" || len(st.Trial.Boots) != 0 || st.FailureCount(sum, "boot-1") != 0 {
		t.Fatalf("want a fresh user trial of the same agent, got %+v failures %+v", st.Trial, st.Failures)
	}
}

func TestAdoptInboxDropsAnAutomaticUpdateFetchedForAnotherAgent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.05-000000"))
	// Fetched while the image agent ran; a user's agent was approved since.
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Replaces: ReplacesImage, Version: "2026.11.05-000000", Build: "build-1"})
	st := State{Approved: &Agent{SHA256: "users", Version: "2026.11.04-000000"}}
	reason, commit := s.adoptInbox(context.Background(), &st, "build-1")
	commit()
	if reason != "" || st.Trial != nil || len(st.Rejected) != 0 {
		t.Fatalf("a stale update is dropped without a verdict: reason %q, state %+v", reason, st)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.Store.InboxDir(), sum+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the stale update must leave the inbox")
	}
}

func TestAdoptInboxTrialsAnAutomaticUpdate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	for _, tc := range []struct {
		name     string
		replaces string
		st       State
	}{
		{"over the approved agent", "users", State{Approved: &Agent{SHA256: "users", Version: "2026.11.04-000000"}}},
		{"over the image agent", ReplacesImage, State{}},
		{"over a rejected agent", ReplacesImage, State{
			Approved: &Agent{SHA256: "users", Version: "2026.11.04-000000"},
			Rejected: []Rejection{{SHA256: "users", Build: "build-1"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newInboxSupervisor(t, printsVersion("2026.11.05-000000"))
			sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Replaces: tc.replaces, RemoteVia: ViaCloud, Version: "2026.11.05-000000", Build: "build-1"})
			st := tc.st
			reason, _ := s.adoptInbox(context.Background(), &st, "build-1")
			if reason != "" || st.Trial == nil || st.Trial.SHA256 != sum || st.Trial.RemoteVia != ViaCloud {
				t.Fatalf("reason %q, trial %+v", reason, st.Trial)
			}
		})
	}
}

func TestAdoptInboxRecordsTheReleaseItRejects(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, func(context.Context, string) (string, error) { return "", errors.New("exit status 2") })
	dropInInbox(t, s.cfg.Store, "a", Incoming{Replaces: ReplacesImage, Version: "2026.11.05-000000", Build: "build-1"})
	var st State
	if reason, _ := s.adoptInbox(context.Background(), &st, "build-1"); reason == "" {
		t.Fatal("the upload must be refused")
	}
	if !st.GivenUp("", "2026.11.05-000000", "build-1") {
		t.Fatalf("the fetcher must learn not to fetch this release again: %+v", st.Rejected)
	}
}

func setMtime(t *testing.T, s Store, sha string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(filepath.Join(s.InboxDir(), sha+".json"), at, at); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptInboxPrefersAUsersUploadOverAnAutomaticOne(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.05-000000"))
	users := dropInInbox(t, s.cfg.Store, "users", Incoming{Explicit: true, RemoteVia: "lan"})
	fetched := dropInInbox(t, s.cfg.Store, "fetched", Incoming{Replaces: ReplacesImage, Build: "build-1"})
	setMtime(t, s.cfg.Store, users, time.Now().Add(-time.Minute))
	setMtime(t, s.cfg.Store, fetched, time.Now())
	var st State
	reason, commit := s.adoptInbox(context.Background(), &st, "build-1")
	commit()
	if reason != "" || st.Trial == nil || st.Trial.SHA256 != users || st.Trial.Automatic {
		t.Fatalf("the user's upload must win: reason %q, trial %+v", reason, st.Trial)
	}
	if entries, _ := os.ReadDir(s.cfg.Store.InboxDir()); len(entries) != 0 {
		t.Fatalf("the automatic update is dropped with it, left %d entries", len(entries))
	}
}

func TestAdoptInboxDropsAStaleAutomaticUpdate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	for _, tc := range []struct {
		name string
		in   Incoming
		st   State
	}{
		{"fetched on another OS build", Incoming{Replaces: ReplacesImage, Build: "build-0"}, State{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newInboxSupervisor(t, printsVersion("2026.11.05-000000"))
			dropInInbox(t, s.cfg.Store, "a", tc.in)
			st := tc.st
			rejected := len(st.Rejected)
			reason, commit := s.adoptInbox(context.Background(), &st, "build-1")
			commit()
			if reason != "" || st.Trial != nil || len(st.Rejected) != rejected {
				t.Fatalf("dropped without a verdict: reason %q, state %+v", reason, st)
			}
		})
	}
}

func TestAdoptInboxDropsAnUploadThatVanished(t *testing.T) {
	s := newInboxSupervisor(t, printsVersion("2026.11.05-000000"))
	if err := os.MkdirAll(s.cfg.Store.InboxDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteIncoming(s.cfg.Store.InboxDir(), Incoming{SHA256: strings.Repeat("a", 64), Version: "2026.11.05-000000", Replaces: ReplacesImage, Build: "build-1"}); err != nil {
		t.Fatal(err)
	}
	var st State
	reason, _ := s.adoptInbox(context.Background(), &st, "build-1")
	if reason != "" || st.Trial != nil || len(st.Rejected) != 0 {
		t.Fatalf("a missing file says nothing about the release: reason %q, state %+v", reason, st)
	}
}

func TestAdoptInboxRejectsAReleaseCarryingAnotherAgent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.04-000000"))
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Version: "2026.11.05-000000", Replaces: ReplacesImage, Build: "build-1"})
	var st State
	if reason, _ := s.adoptInbox(context.Background(), &st, "build-1"); reason == "" || st.Trial != nil {
		t.Fatalf("the release must hold the agent it names: reason %q", reason)
	}
	if !st.GivenUp(sum, "2026.11.05-000000", "build-1") {
		t.Fatalf("rejected = %+v", st.Rejected)
	}
}

func TestAdoptInboxMarksAnAutomaticTrial(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.05-000000"))
	dropInInbox(t, s.cfg.Store, "a", Incoming{Version: "2026.11.05-000000", Replaces: ReplacesImage, Build: "build-1", RemoteVia: ViaCloud})
	var st State
	if _, _ = s.adoptInbox(context.Background(), &st, "build-1"); st.Trial == nil || !st.Trial.Automatic {
		t.Fatalf("trial = %+v", st.Trial)
	}
}

func TestClearInboxSparesARunningFetch(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"abc.json", "abc", ".fetch-archive-1", ".fetch-archive-old"} {
		os.WriteFile(filepath.Join(dir, name), nil, 0o644)
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(filepath.Join(dir, ".fetch-archive-old"), old, old)
	clearInbox(dir)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != ".fetch-archive-1" {
		t.Fatalf("left %v", entries)
	}
}

func TestAdoptInboxSkipsAnUnreadableUpload(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.05-000000"))
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Replaces: ReplacesImage, Build: "build-1", Version: "2026.11.05-000000"})
	broken := filepath.Join(s.cfg.Store.InboxDir(), strings.Repeat("d", 64)+".json")
	os.WriteFile(broken, []byte("{"), 0o644)
	os.Chtimes(broken, time.Now().Add(-time.Minute), time.Now().Add(-time.Minute))
	var st State
	reason, _ := s.adoptInbox(context.Background(), &st, "build-1")
	if reason != "" || st.Trial == nil || st.Trial.SHA256 != sum {
		t.Fatalf("a broken leftover must not cost the good upload: reason %q, trial %+v", reason, st.Trial)
	}
}

func TestAdoptInboxRetriesAnUnconfirmedAgentAFewTimes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	for misses := 1; misses <= MaxUnconfirmed; misses++ {
		s := newInboxSupervisor(t, printsVersion("2026.11.05-000000"))
		sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Replaces: ReplacesImage, Build: "build-1", Version: "2026.11.05-000000"})
		var st State
		for i := 0; i < misses; i++ {
			st.RejectUnconfirmed(sum, "2026.11.05-000000", "build-1", "the new agent has not reached the cloud yet")
		}
		reason, _ := s.adoptInbox(context.Background(), &st, "build-1")
		if retried := st.Trial != nil; retried != (misses < MaxUnconfirmed) {
			t.Fatalf("after %d missed confirms: trial %v, reason %q", misses, st.Trial, reason)
		}
	}
}

func TestAdoptInboxRemovesADanglingUpload(t *testing.T) {
	s := newInboxSupervisor(t, printsVersion("2026.11.05-000000"))
	os.MkdirAll(s.cfg.Store.InboxDir(), 0o755)
	link := filepath.Join(s.cfg.Store.InboxDir(), strings.Repeat("e", 64)+".json")
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), link); err != nil {
		t.Fatal(err)
	}
	var st State
	if reason, _ := s.adoptInbox(context.Background(), &st, "build-1"); reason != "" || st.Trial != nil {
		t.Fatalf("reason %q, trial %+v", reason, st.Trial)
	}
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a dangling upload would wake the supervisor forever")
	}
}

func TestAdoptInboxEndsTheGraceOfAConfirmedTrial(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.05-000000"))
	next := dropInInbox(t, s.cfg.Store, "next", Incoming{Explicit: true, RemoteVia: "lan"})
	st := State{Trial: &Trial{Agent: Agent{SHA256: "first", Version: "2026.11.04-000000"}, Confirmed: true, Build: "build-1"}}
	reason, _ := s.adoptInbox(context.Background(), &st, "build-1")
	if reason != "" || st.Trial == nil || st.Trial.SHA256 != next || st.Approved == nil || st.Approved.SHA256 != "first" {
		t.Fatalf("the confirmed agent becomes the fallback for the next upload: reason %q, state %+v", reason, st)
	}
}

func TestAdoptInboxJudgesAReCheckAgainWhenTheUserSendsIt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.05-000000"))
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Explicit: true, RemoteVia: "lan"})
	st := State{Trial: &Trial{Agent: Agent{SHA256: sum, Version: "2026.11.05-000000"}, Revalidation: true, Confirmed: true, Build: "build-1"}}
	s.adoptInbox(context.Background(), &st, "build-1")
	if st.Trial == nil || st.Trial.Confirmed || st.Trial.RemoteVia != "lan" {
		t.Fatalf("the user's update needs their own confirm and verdict: %+v", st.Trial)
	}
}

func TestAdoptInboxSkipsASeedThatIsNotNewer(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	// The image's agent is 2026.11.05-114424 (a nightly).
	for _, tc := range []struct {
		name, seed, image string
		st                State
		trial             bool
	}{
		{"an older stable seed on a nightly image", "2026.11.04-081952", "", State{}, false},
		{"the same agent as the image", "2026.11.05-114424", "", State{}, false},
		{"a newer agent than the image", "2026.11.06-000000", "", State{}, true},
		{"a dev build", "2026.11.01-000000-dev", "", State{}, true},
		{"a dev build on a dev image", "2026.11.01-000000-dev", "dev", State{}, true},
		{"older than the approved agent on /data", "2026.11.06-000000", "", State{
			Approved: &Agent{SHA256: "users", Version: "2026.11.07-000000", Builds: []string{"build-1"}},
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newInboxSupervisor(t, printsVersion(tc.seed))
			if tc.image != "" {
				s.cfg.ImageVersion = tc.image
			}
			dropInInbox(t, s.cfg.Store, "seed", Incoming{Seed: true})
			st := tc.st
			reason, commit := s.adoptInbox(context.Background(), &st, "build-1")
			commit()
			if got := st.Trial != nil; got != tc.trial || reason != "" || len(st.Rejected) != 0 {
				t.Fatalf("trial %v (want %v), reason %q, rejected %v", got, tc.trial, reason, st.Rejected)
			}
			if entries, _ := os.ReadDir(s.cfg.Store.InboxDir()); !tc.trial && len(entries) != 0 {
				t.Fatalf("a skipped seed must leave the inbox, left %d entries", len(entries))
			}
		})
	}
}

func TestRestoreLeavesWhatIsNotOnTheListAlone(t *testing.T) {
	src := filepath.Join(t.TempDir(), "wendy-agent")
	os.MkdirAll(filepath.Join(src, "cloud-relay"), 0o700)
	os.MkdirAll(filepath.Join(src, "mesh"), 0o700)
	write := func(rel, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(src, rel), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(rel string) string {
		data, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			return "<missing>"
		}
		return string(data)
	}
	write("local-mesh.json", "old mesh")
	write("mesh-disabled", "")
	write("mesh/a", "old a")
	write("provisioning.json", "unenrolled")
	write("cloud-relay/lease.pb", "L1")
	write("postbox.json", "cursor 1")
	dirs := []SnapshotDir{{Path: src, Only: []string{"local-mesh.json", "nan-mesh.json", "mesh-disabled", "mesh"}}}
	snap := filepath.Join(t.TempDir(), "snapshot")
	if err := snapshot(snap, dirs); err != nil {
		t.Fatal(err)
	}

	// The trial changes settings and also enrolls, renews its lease and moves its cursor.
	write("local-mesh.json", "new format")
	write("nan-mesh.json", "added by the trial")
	os.Remove(filepath.Join(src, "mesh-disabled"))
	write("mesh/b", "added by the trial")
	write("provisioning.json", "enrolled")
	write("device-key.pem", "key")
	write("cloud-relay/lease.pb", "L2")
	write("postbox.json", "cursor 2")
	if err := restore(snap, dirs); err != nil {
		t.Fatalf("restore: %v", err)
	}

	for rel, want := range map[string]string{
		"local-mesh.json":      "old mesh",
		"nan-mesh.json":        "<missing>",
		"mesh-disabled":        "",
		"mesh/a":               "old a",
		"mesh/b":               "<missing>",
		"provisioning.json":    "enrolled",
		"device-key.pem":       "key",
		"cloud-relay/lease.pb": "L2",
		"postbox.json":         "cursor 2",
	} {
		if got := read(rel); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

func TestRestoreRemovesAListedFileThatAppearedInAMissingDir(t *testing.T) {
	src := filepath.Join(t.TempDir(), "wendy-agent")
	dirs := []SnapshotDir{{Path: src, Only: []string{"build-host-enabled"}}}
	snap := filepath.Join(t.TempDir(), "snapshot")
	if err := snapshot(snap, dirs); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(src, 0o700)
	os.WriteFile(filepath.Join(src, "build-host-enabled"), nil, 0o600)
	os.WriteFile(filepath.Join(src, "provisioning.json"), []byte("enrolled"), 0o600)
	if err := restore(snap, dirs); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, "build-host-enabled")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a listed file the trial added must go")
	}
	if _, err := os.Stat(filepath.Join(src, "provisioning.json")); err != nil {
		t.Fatal("an unlisted file must stay")
	}
}

// dropProgramInInbox stages a copy of a system program, which lacks the
// agent contract, as an upload.
func dropProgramInInbox(t *testing.T, s Store, meta Incoming) string {
	t.Helper()
	path, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1) on this host")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(data)
	meta.SHA256 = hex.EncodeToString(h[:])
	os.MkdirAll(s.InboxDir(), 0o755)
	if err := os.WriteFile(filepath.Join(s.InboxDir(), meta.SHA256), data, 0o755); err != nil {
		t.Fatal(err)
	}
	m, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(s.InboxDir(), meta.SHA256+".json"), m, 0o644); err != nil {
		t.Fatal(err)
	}
	return meta.SHA256
}

func TestAdoptInboxSkipsASeedWithoutTheContract(t *testing.T) {
	s := newInboxSupervisor(t, printsVersion("2026.11.02-101010"))
	dropProgramInInbox(t, s.cfg.Store, Incoming{Seed: true})
	var st State
	reason, commit := s.adoptInbox(context.Background(), &st, "build-1")
	commit()
	// Seeds from CLIs and releases before the supervisor are expected, not failures.
	if reason != "" || len(st.Rejected) != 0 || st.Trial != nil {
		t.Fatalf("want a quiet skip, got reason %q, state %+v", reason, st)
	}
	if left, _ := os.ReadDir(s.cfg.Store.InboxDir()); len(left) != 0 {
		t.Fatalf("the seed must be dropped, inbox has %d entries", len(left))
	}
}

func TestAdoptInboxRejectsAnUploadWithoutTheContractOnce(t *testing.T) {
	s := newInboxSupervisor(t, printsVersion("2026.11.02-101010"))
	sum := dropProgramInInbox(t, s.cfg.Store, Incoming{Explicit: true})
	var st State
	reason, commit := s.adoptInbox(context.Background(), &st, "build-1")
	commit()
	if !st.IsRejected(sum, "build-1") || !strings.Contains(reason, "supervisor") {
		t.Fatalf("an upload without the contract must be rejected, got %q", reason)
	}
	// The CLI frames the reason as "the new agent failed its checks: <reason>".
	if strings.Contains(reason, "failed its checks") {
		t.Fatalf("the reason repeats the CLI's framing: %q", reason)
	}
}

func TestAWholeTreeRestoreLeavesAnAllowlistSnapshotAlone(t *testing.T) {
	src := filepath.Join(t.TempDir(), "wendy-agent")
	os.MkdirAll(src, 0o700)
	os.WriteFile(filepath.Join(src, "local-mesh.json"), []byte("mesh"), 0o600)
	os.WriteFile(filepath.Join(src, "provisioning.json"), []byte("enrolled"), 0o600)
	snap := filepath.Join(t.TempDir(), "snapshot")
	if err := snapshot(snap, []SnapshotDir{{Path: src, Only: []string{"local-mesh.json"}}}); err != nil {
		t.Fatal(err)
	}
	// An older supervisor, after an OS rollback, restores whole trees.
	if err := restore(snap, []SnapshotDir{{Path: src}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "provisioning.json")); err != nil {
		t.Fatal("a whole-tree restore of an allowlist snapshot deleted the identity")
	}
}

func TestRestoreLeavesANameTheSnapshotDidNotCapture(t *testing.T) {
	src := filepath.Join(t.TempDir(), "wendy-agent")
	os.MkdirAll(src, 0o700)
	os.WriteFile(filepath.Join(src, "build-host-enabled"), nil, 0o600)
	snap := filepath.Join(t.TempDir(), "snapshot")
	if err := snapshot(snap, []SnapshotDir{{Path: src, Only: []string{"local-mesh.json"}}}); err != nil {
		t.Fatal(err)
	}
	// A later release lists more names than this snapshot captured.
	if err := restore(snap, []SnapshotDir{{Path: src, Only: []string{"local-mesh.json", "build-host-enabled"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "build-host-enabled")); err != nil {
		t.Fatal("a name the snapshot never captured was removed")
	}
}

func TestClearInboxKeepsTheCloudDeferrals(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	os.MkdirAll(s.InboxDir(), 0o755)
	os.WriteFile(s.CloudDeferralsPath(), []byte("2"), 0o644)
	os.WriteFile(filepath.Join(s.InboxDir(), "abc.json"), []byte("{}"), 0o644)
	clearInbox(s.InboxDir())
	// The fetcher owns the count; a user's upload must not reset its wait.
	if data, err := os.ReadFile(s.CloudDeferralsPath()); err != nil || string(data) != "2" {
		t.Fatalf("the deferral count was cleared: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(s.InboxDir(), "abc.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the upload must still be cleared")
	}
}

func TestAReSentApprovedAgentRepairsItsFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs an ELF test binary")
	}
	s := newInboxSupervisor(t, printsVersion("2026.11.02-101010"))
	sum := dropInInbox(t, s.cfg.Store, "a", Incoming{Explicit: true})
	good, _ := os.ReadFile(filepath.Join(s.cfg.Store.InboxDir(), sum))
	os.MkdirAll(filepath.Dir(s.cfg.Store.BinPath(sum)), 0o755)
	os.WriteFile(s.cfg.Store.BinPath(sum), []byte("damaged"), 0o755)
	st := State{Approved: &Agent{SHA256: sum, Version: "2026.11.02-101010", Builds: []string{"build-1"}}}
	_, commit := s.adoptInbox(context.Background(), &st, "build-1")
	commit()
	got, _ := os.ReadFile(s.cfg.Store.BinPath(sum))
	if !bytes.Equal(got, good) {
		t.Fatal("the re-sent agent must replace a damaged copy of itself")
	}
}
