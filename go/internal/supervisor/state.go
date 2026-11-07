// Package supervisor decides which wendy-agent a WendyOS device runs: a new
// agent on trial, the approved one on /data, or the image's own copy.
package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

// maxRejected bounds the rejection history; only recent builds matter.
const maxRejected = 20

// MaxUnconfirmed is how many trials of one agent may fail only their remote
// confirm on a build before an automatic update stops retrying it.
const MaxUnconfirmed = 3

// Store is the agent store on /data. Binaries are named by their sha256 and
// never moved, so a running agent's path stays valid when it is promoted.
type Store struct {
	Dir string
}

func (s Store) BinPath(sha string) string { return filepath.Join(s.Dir, "bin", sha) }
func (s Store) InboxDir() string          { return filepath.Join(s.Dir, "inbox") }
func (s Store) SnapshotDir() string       { return filepath.Join(s.Dir, "snapshot") }
func (s Store) StatePath() string         { return filepath.Join(s.Dir, "state.json") }

// CloudDeferralsPath counts the fetch runs in a row that held an update back
// for the cloud. It is in the inbox, the only place the updater may write.
const cloudDeferralsName = ".cloud-deferrals"

func (s Store) CloudDeferralsPath() string { return filepath.Join(s.InboxDir(), cloudDeferralsName) }

type Agent struct {
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
	// Builds lists the OS builds this agent passed its checks on.
	Builds []string `json:"validated_builds,omitempty"`
}

type Trial struct {
	Agent
	// RemoteVia is the path the new agent must be reached on before it is
	// approved: "lan" or "relay" for the client that sent it, ViaCloud for an
	// automatic update.
	RemoteVia string `json:"remote_via,omitempty"`
	// Automatic marks a trial the daily update started; no client waits on it.
	Automatic bool `json:"automatic,omitempty"`
	// Revalidation re-checks an approved agent on a new OS build.
	Revalidation bool `json:"revalidation,omitempty"`
	// Confirmed: it passed its checks on Build and is reported approved; until its
	// deadline it can still be rejected, so the fallback and the snapshot stay.
	Confirmed bool   `json:"confirmed,omitempty"`
	Build     string `json:"build,omitempty"`
	// Boots lists the boots this trial ran in; StartedAt is the boot-clock
	// time it started in the last one. The deadline spans all its runs in a boot.
	Boots     []string      `json:"boots,omitempty"`
	StartedAt time.Duration `json:"started_at,omitempty"`
}

type Rejection struct {
	SHA256  string `json:"sha256"`
	Version string `json:"version,omitempty"`
	Build   string `json:"build"`
	Reason  string `json:"reason"`
	// Unconfirmed means the agent passed its local checks and only the
	// remote confirm was missing, which a network blip can cause.
	Unconfirmed bool `json:"unconfirmed,omitempty"`
}

type Failure struct {
	SHA256 string `json:"sha256"`
	BootID string `json:"boot_id"`
	Count  int    `json:"count"`
}

// RemoteSeenDir is where the agent marks that a remote client reached it.
const RemoteSeenDir = "/run/wendy-agent"

// ViaCloud is the agent's own connection to the cloud relay. It confirms an
// automatic update, which no client is waiting on.
const ViaCloud = "cloud"

// ReplacesImage is Incoming.Replaces for an update fetched while no agent on
// /data was approved.
const ReplacesImage = "image"

// Incoming is the metadata an agent writes next to an upload. The .json is
// written last, so its presence means the binary is complete.
type Incoming struct {
	SHA256 string `json:"sha256"`
	// RemoteVia is "lan" or "relay" for an upload from a remote client,
	// ViaCloud for an automatic update, "" for a local one.
	RemoteVia string `json:"remote_via,omitempty"`
	// Explicit uploads (a user ran an update) retry an agent rejected before.
	Explicit bool `json:"explicit,omitempty"`
	// Version is the release an automatic update came from.
	Version string `json:"version,omitempty"`
	// Replaces is the approved agent an automatic update was fetched for, or
	// ReplacesImage; Build is the OS build it was fetched on. It is dropped if
	// either changed in the meantime.
	Replaces string `json:"replaces,omitempty"`
	Build    string `json:"build,omitempty"`
	// Seed is the agent `wendy os install` left on /config.
	Seed bool `json:"seed,omitempty"`
}

// State is written only by the supervisor.
type State struct {
	Approved *Agent      `json:"approved,omitempty"`
	Trial    *Trial      `json:"trial,omitempty"`
	Rejected []Rejection `json:"rejected,omitempty"`
	Failures []Failure   `json:"failures,omitempty"`
}

// ErrCorruptState means state.json exists but cannot be parsed.
var ErrCorruptState = errors.New("agent state is corrupt")

// LoadState returns an empty state when the file does not exist yet.
func LoadState(path string) (State, error) {
	var st State
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("%w: %s: %v", ErrCorruptState, path, err)
	}
	return st, nil
}

func SaveState(path string, st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, data, 0o644)
}

func (st State) IsRejected(sha, build string) bool {
	for _, r := range st.Rejected {
		if r.SHA256 == sha && r.Build == build {
			return true
		}
	}
	return false
}

// hardRejected reports a real failure of sha on build, not only a missed
// remote confirm.
func (st State) hardRejected(sha, build string) bool {
	return slices.ContainsFunc(st.Rejected, func(r Rejection) bool {
		return !r.Unconfirmed && r.SHA256 == sha && r.Build == build
	})
}

// rejectionReason is the latest reason of a real failure of sha on build.
func (st State) rejectionReason(sha, build string) string {
	for i := len(st.Rejected) - 1; i >= 0; i-- {
		if r := st.Rejected[i]; !r.Unconfirmed && r.SHA256 == sha && r.Build == build {
			return r.Reason
		}
	}
	return ""
}

// GivenUp reports whether an automatic update should leave this agent alone
// on this build: it failed there, or it missed its remote confirm too often.
// Match it by sha256, by release version, or both.
func (st State) GivenUp(sha, version, build string) bool {
	version = strings.TrimPrefix(version, "v")
	unconfirmed := 0
	for _, r := range st.Rejected {
		bySHA := sha != "" && r.SHA256 == sha
		byVersion := version != "" && strings.TrimPrefix(r.Version, "v") == version
		if r.Build != build || !(bySHA || byVersion) {
			continue
		}
		if !r.Unconfirmed {
			return true
		}
		unconfirmed++
	}
	return unconfirmed >= MaxUnconfirmed
}

// Replaceable is what an automatic update fetched now would replace: the
// approved agent's sha256, or ReplacesImage.
func (st State) Replaceable(build string) string {
	if a := st.Approved; a != nil && !st.IsRejected(a.SHA256, build) {
		return a.SHA256
	}
	return ReplacesImage
}

// staleAutomatic reports an automatic upload fetched for an agent or an OS
// build that has changed since.
func staleAutomatic(in Incoming, st State, build string) bool {
	return in.Replaces != "" && (in.Build != build || in.Replaces != st.Replaceable(build))
}

func (st *State) Reject(sha, version, build, reason string) {
	st.reject(Rejection{SHA256: sha, Version: version, Build: build, Reason: reason})
}

// RejectUnconfirmed records a trial that missed only its remote confirm.
func (st *State) RejectUnconfirmed(sha, version, build, reason string) {
	st.reject(Rejection{SHA256: sha, Version: version, Build: build, Reason: reason, Unconfirmed: true})
}

func (st *State) reject(r Rejection) {
	if !r.Unconfirmed {
		// One hard entry per agent and build; unconfirmed misses are counted.
		// A clone, so a caller's copy of the state is left alone.
		st.Rejected = slices.DeleteFunc(slices.Clone(st.Rejected), func(old Rejection) bool {
			same := !old.Unconfirmed && old.SHA256 == r.SHA256 && old.Build == r.Build
			if same && r.Version == "" {
				// An explicit re-send has no version; the fetcher matches by it.
				r.Version = old.Version
			}
			return same
		})
	}
	st.Rejected = append(st.Rejected, r)
	if n := len(st.Rejected); n > maxRejected {
		st.Rejected = st.Rejected[n-maxRejected:]
	}
}

// RecordFailure counts a failed run of sha in this boot and forgets earlier
// boots, so a reboot gives every agent a fresh budget.
func (st *State) RecordFailure(sha, bootID string) {
	kept := st.Failures[:0]
	for _, f := range st.Failures {
		if f.BootID == bootID {
			kept = append(kept, f)
		}
	}
	st.Failures = kept
	for i := range st.Failures {
		if st.Failures[i].SHA256 == sha {
			st.Failures[i].Count++
			return
		}
	}
	st.Failures = append(st.Failures, Failure{SHA256: sha, BootID: bootID, Count: 1})
}

// Promote makes the trial the approved agent, validated on build.
func (st *State) Promote(build string) {
	a := st.Trial.Agent
	if build != "" {
		a.Builds = append(slices.Clone(a.Builds), build)
	}
	st.Approved, st.Trial = &a, nil
	// A retried agent may carry a rejection or crashes from an earlier attempt,
	// and its approval proves it can be reached, wherever it missed that before.
	st.Rejected = slices.DeleteFunc(slices.Clone(st.Rejected), func(r Rejection) bool {
		return r.SHA256 == a.SHA256 && (r.Build == build || r.Unconfirmed)
	})
	st.Failures = slices.Clone(st.Failures)
	st.ClearFailures(a.SHA256)
}

func (st *State) ClearFailures(sha string) {
	st.Failures = slices.DeleteFunc(st.Failures, func(f Failure) bool { return f.SHA256 == sha })
}

func (st State) FailureCount(sha, bootID string) int {
	for _, f := range st.Failures {
		if f.SHA256 == sha && f.BootID == bootID {
			return f.Count
		}
	}
	return 0
}
