//go:build linux

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"go.uber.org/zap"
)

// adoptInbox turns the newest upload into a trial. It returns why an upload
// was refused ("" otherwise) and a commit func that clears the inbox; call it
// only once the state is saved, so a failed save keeps the upload for a retry.
func (s *Supervisor) adoptInbox(ctx context.Context, st *State, build string) (string, func()) {
	inbox := s.cfg.Store.InboxDir()
	keep := func() {}
	metas, _ := filepath.Glob(filepath.Join(inbox, "*.json"))
	if len(metas) == 0 {
		return "", keep
	}
	commit := func() { clearInbox(inbox) }
	in, err := pickUpload(metas)
	s.uploadSHA = in.SHA256
	if errors.Is(err, errNoUpload) {
		// Removed while read, or a link to nowhere: clear only what is left of
		// them, never an upload that arrived since.
		for _, m := range metas {
			if _, err := os.Lstat(m); err == nil {
				os.Remove(m)
			}
		}
		return "", keep
	}
	if err != nil {
		return err.Error(), commit
	}
	automatic := in.Replaces != ""
	if staleAutomatic(in, *st, build) {
		// Fetched for an agent or an OS build that has changed since: the
		// next fetch decides again.
		return "", commit
	}
	if t := st.Trial; t != nil && t.Confirmed && t.SHA256 != in.SHA256 {
		// It passed its checks and a new upload follows: its grace ends here,
		// and it becomes the fallback for the next trial.
		st.Promote(t.Build)
	}
	if t := st.Trial; t != nil {
		switch {
		case t.SHA256 == in.SHA256:
			if in.Explicit && t.Revalidation {
				// The user sent it again: judge it as their update, with a verdict.
				t.Revalidation, t.RemoteVia, t.Boots, t.Confirmed = false, in.RemoteVia, nil, false
				st.ClearFailures(in.SHA256)
			}
			return "", commit
		case !t.Revalidation || !in.Explicit:
			return "", keep
		}
		// A user's update replaces the re-check of the approved agent.
		st.Trial = nil
	}
	if a := st.Approved; a != nil && a.SHA256 == in.SHA256 && !st.IsRejected(in.SHA256, build) {
		if in.Explicit {
			// A user asked for it again: another chance after crashes this boot,
			// and its file replaced in case the copy on disk was damaged.
			st.ClearFailures(in.SHA256)
			s.reapproved = in.SHA256
			s.repairBinary(in.SHA256)
		}
		return "", commit
	}
	if !in.Explicit && st.GivenUp(in.SHA256, in.Version, build) {
		return "this agent already failed its checks on this OS build", commit
	}

	upload, bin := filepath.Join(inbox, in.SHA256), s.cfg.Store.BinPath(in.SHA256)
	path := upload
	if _, err := os.Stat(upload); errors.Is(err, os.ErrNotExist) {
		// An earlier pass moved it, then could not save the trial.
		path = bin
		if _, err := os.Stat(bin); errors.Is(err, os.ErrNotExist) {
			// Removed before it was complete; that says nothing about the agent.
			return "", commit
		}
	}
	if path == upload {
		// The check runs it, and a file without an x bit cannot run, even as root.
		if err := os.Chmod(upload, 0o755); err != nil {
			return fmt.Sprintf("preparing the new agent: %v", err), keep
		}
	}
	ver, err := Check(ctx, path, Want{SHA256: in.SHA256, Arch: s.cfg.Arch}, s.deps.RunVersion)
	if err != nil && ctx.Err() != nil {
		// Stopped mid-check: not a verdict, check it again on the next start.
		return "", keep
	}
	if err == nil && in.Version != "" && strings.TrimPrefix(ver, "v") != strings.TrimPrefix(in.Version, "v") {
		err = fmt.Errorf("release %s holds agent %s", in.Version, ver)
	}
	if err != nil {
		if in.Seed && errors.Is(err, ErrNoContract) {
			// Older CLIs and releases still seed such agents.
			s.deps.Log.Info("skipping the /config agent: it predates the agent supervisor")
			return "", commit
		}
		reason := err.Error()
		s.deps.Log.Warn("new agent failed its checks", zap.String("sha256", in.SHA256), zap.String("reason", reason))
		st.Reject(in.SHA256, in.Version, build, reason)
		return reason, commit
	}
	if current := s.runningVersion(*st, build); in.Seed && !version.IsDev(ver) && version.CompareVersions(ver, current) <= 0 {
		// `wendy os install` seeds the latest stable agent even onto a newer
		// (nightly) image; the newer agent wins, as after an OS update.
		s.deps.Log.Info("skipping the /config agent: the device already runs the same or a newer one",
			zap.String("seed", ver), zap.String("running", current))
		return "", commit
	}
	if path == upload {
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			return fmt.Sprintf("preparing the agent store: %v", err), keep
		}
		if err := os.Rename(upload, bin); err != nil {
			return fmt.Sprintf("storing the new agent: %v", err), keep
		}
	}
	if in.Explicit {
		st.ClearFailures(in.SHA256)
	}
	st.Trial = &Trial{Agent: Agent{SHA256: in.SHA256, Version: ver}, RemoteVia: in.RemoteVia, Automatic: automatic}
	return "", commit
}

// runningVersion is the agent the device runs without this upload: the
// approved one, or the image's.
func (s *Supervisor) runningVersion(st State, build string) string {
	if st.Replaceable(build) != ReplacesImage {
		return st.Approved.Version
	}
	return s.cfg.ImageVersion
}

// pickUpload takes the newest upload a person made (a CLI push or the /config
// seed), and the newest automatic one only when there is none. Unreadable
// metadata is skipped; it fails only when nothing else is there.
func pickUpload(metas []string) (Incoming, error) {
	sort.Slice(metas, func(i, j int) bool { return modTime(metas[i]) > modTime(metas[j]) })
	var picked *Incoming
	unreadable := ""
	for _, m := range metas {
		var in Incoming
		data, err := os.ReadFile(m)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil {
			err = json.Unmarshal(data, &in)
		}
		if err != nil || in.SHA256 != strings.TrimSuffix(filepath.Base(m), ".json") {
			unreadable = filepath.Base(m)
			continue
		}
		if in.Replaces == "" {
			return in, nil
		}
		if picked == nil {
			picked = &in
		}
	}
	switch {
	case picked != nil:
		return *picked, nil
	case unreadable != "":
		return Incoming{}, fmt.Errorf("unreadable upload metadata %s", unreadable)
	}
	return Incoming{}, errNoUpload
}

// errNoUpload: every upload vanished while it was read.
var errNoUpload = errors.New("no upload")

// fetchGrace keeps a running fetch's partial download from being cleared.
const fetchGrace = time.Hour

func clearInbox(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if e.Name() == cloudDeferralsName {
			// The fetcher owns it; an upload must not restart its wait.
			continue
		}
		if strings.HasPrefix(e.Name(), fetchTempPrefix) {
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) < fetchGrace {
				continue
			}
		}
		os.RemoveAll(path)
	}
}

func modTime(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

// uploadTempPattern is the agent's in-progress upload file; with no agent
// running, one left in the inbox was abandoned by an agent that died.
const uploadTempPattern = ".agent-update-*"

func removeAbandonedUploads(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, uploadTempPattern))
	for _, m := range matches {
		os.Remove(m)
	}
}

// repairBinary puts a re-sent agent's verified upload in place of its stored
// file. Renaming over a running binary is safe.
func (s *Supervisor) repairBinary(sha string) {
	upload := filepath.Join(s.cfg.Store.InboxDir(), sha)
	if got, err := fileSHA256(upload); err != nil || got != sha {
		return
	}
	if err := os.Chmod(upload, 0o755); err != nil {
		return
	}
	if err := os.Rename(upload, s.cfg.Store.BinPath(sha)); err != nil {
		s.deps.Log.Warn("could not replace the stored agent", zap.Error(err))
	}
}
