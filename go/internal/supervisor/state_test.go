package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadStateOfAMissingFileIsEmpty(t *testing.T) {
	st, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.Approved != nil || st.Trial != nil || len(st.Rejected) != 0 || len(st.Failures) != 0 {
		t.Fatalf("want empty state, got %+v", st)
	}
}

func TestSaveStateRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := State{
		Approved: &Agent{SHA256: "aa", Version: "2026.10.30-154553", Builds: []string{"b1"}},
		Trial:    &Trial{Agent: Agent{SHA256: "bb", Version: "2026.11.01-000000"}, RemoteVia: "relay"},
		Rejected: []Rejection{{SHA256: "cc", Build: "b1", Reason: "crashed"}},
		Failures: []Failure{{SHA256: "bb", BootID: "boot1", Count: 2}},
	}
	if err := SaveState(path, want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Approved.SHA256 != "aa" || got.Approved.Builds[0] != "b1" ||
		got.Trial.SHA256 != "bb" || got.Trial.RemoteVia != "relay" ||
		got.Rejected[0].Reason != "crashed" || got.Failures[0].Count != 2 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestLoadStateRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("want ErrCorruptState for a corrupt state file, got %v", err)
	}
}

func TestStoreLayout(t *testing.T) {
	s := Store{Dir: "/data/wendy-agent/agent"}
	if got := s.BinPath("abc"); got != "/data/wendy-agent/agent/bin/abc" {
		t.Errorf("BinPath = %q", got)
	}
	if got := s.InboxDir(); got != "/data/wendy-agent/agent/inbox" {
		t.Errorf("InboxDir = %q", got)
	}
	if got := s.StatePath(); got != "/data/wendy-agent/agent/state.json" {
		t.Errorf("StatePath = %q", got)
	}
}

func TestClearFailures(t *testing.T) {
	var st State
	st.RecordFailure("a", "boot-1")
	st.RecordFailure("b", "boot-1")
	st.ClearFailures("a")
	if st.FailureCount("a", "boot-1") != 0 || st.FailureCount("b", "boot-1") != 1 {
		t.Fatalf("failures = %+v", st.Failures)
	}
}

func TestGivenUp(t *testing.T) {
	var st State
	st.Reject("a", "2026.11.05-101010", "b1", "crashed")
	if !st.GivenUp("", "2026.11.05-101010", "b1") || !st.GivenUp("", "v2026.11.05-101010", "b1") || !st.GivenUp("a", "", "b1") {
		t.Fatal("a release that failed on this build is not retried, by version or by sha")
	}
	if st.GivenUp("", "2026.11.05-101010", "b2") || st.GivenUp("", "", "b1") {
		t.Fatal("a rejection holds only on its build, and an unknown release matches nothing")
	}
	// Missing only the remote confirm says little about the agent: retry a few times.
	for i := 0; i < MaxUnconfirmed; i++ {
		if st.GivenUp("c", "2026.11.06-000000", "b1") {
			t.Fatalf("given up after %d unconfirmed trials", i)
		}
		st.RejectUnconfirmed("c", "2026.11.06-000000", "b1", "the new agent has not reached the cloud yet")
	}
	if !st.GivenUp("c", "", "b1") || !st.GivenUp("", "2026.11.06-000000", "b1") {
		t.Fatal("an agent that never gets confirmed must stop being retried")
	}
	if !st.IsRejected("c", "b1") {
		t.Fatal("an unconfirmed trial is still a rejection")
	}
}

func TestCloudEnrolledMark(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	enrolled := CloudEnrolledFunc(dir)
	if enrolled(os.Getpid()) {
		t.Fatal("no mark, no enrollment")
	}
	if err := MarkCloudEnrolled(dir); err != nil {
		t.Fatal(err)
	}
	if !enrolled(os.Getpid()) || enrolled(os.Getpid()+1) {
		t.Fatal("the mark belongs to the agent that wrote it")
	}
}

func TestARepeatedRejectionIsRecordedOnce(t *testing.T) {
	var st State
	st.Reject("bad", "1.0", "build-1", "failed 3 times during its trial")
	st.Reject("other", "2.0", "build-1", "failed 3 times during its trial")
	st.Reject("bad", "1.0", "build-1", "failed 3 times during its trial")
	// Repeats must not push other releases' records out of the capped list.
	if len(st.Rejected) != 2 || st.Rejected[1].SHA256 != "bad" {
		t.Fatalf("want one entry per agent and build, newest last: %+v", st.Rejected)
	}
	st.RejectUnconfirmed("shy", "3.0", "build-1", "not reached")
	st.RejectUnconfirmed("shy", "3.0", "build-1", "not reached")
	n := 0
	for _, r := range st.Rejected {
		if r.SHA256 == "shy" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("each unconfirmed miss is its own entry, got %d", n)
	}
	// A later miss of the same agent must not erase that it really failed.
	st.RejectUnconfirmed("bad", "1.0", "build-1", "not reached")
	if !st.GivenUp("bad", "", "build-1") {
		t.Fatal("the hard rejection was lost")
	}
}

func TestARepeatedRejectionKeepsTheReleaseVersion(t *testing.T) {
	var st State
	st.Reject("S", "2026.11.02-101010", "B", "failed 3 times during its trial")
	// An explicit re-send carries no version; the fetcher still matches by it.
	st.Reject("S", "", "B", "--version failed: signal: killed")
	if len(st.Rejected) != 1 || !st.GivenUp("", "2026.11.02-101010", "B") {
		t.Fatalf("the release version was lost: %+v", st.Rejected)
	}
}

func TestRejectLeavesTheCallersStateAlone(t *testing.T) {
	in := State{Rejected: []Rejection{{SHA256: "T", Build: "B", Reason: "old"}, {SHA256: "X", Build: "B", Reason: "x"}}}
	before := slices.Clone(in.Rejected)
	st := in
	st.Reject("T", "1.0", "B", "new")
	if !slices.Equal(in.Rejected, before) {
		t.Fatalf("the caller's rejections changed: %+v", in.Rejected)
	}
}

func TestPromoteForgetsMissedConfirmsOnEveryBuild(t *testing.T) {
	var st State
	st.RejectUnconfirmed("a", "2.0", "build-x", "not reached")
	st.Reject("a", "2.0", "build-y", "failed 3 times during its trial")
	st.Reject("a", "2.0", "build-z", "crashed in an earlier attempt")
	st.RejectUnconfirmed("b", "3.0", "build-x", "not reached")
	st.Trial = &Trial{Agent: Agent{SHA256: "a", Version: "2.0"}, Confirmed: true}
	st.Promote("build-z")
	// Its approval proves it can be reached; a real failure elsewhere still stands.
	if st.IsRejected("a", "build-z") || st.IsRejected("a", "build-x") || !st.IsRejected("a", "build-y") || !st.IsRejected("b", "build-x") {
		t.Fatalf("rejections after the approval: %+v", st.Rejected)
	}
}
