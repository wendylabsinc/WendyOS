package supervisor

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	build = "build-1"
	boot  = "boot-1"
)

func approved(version string, builds ...string) *Agent {
	return &Agent{SHA256: "approved-sha", Version: version, Builds: builds}
}

func trial(sha string) *Trial {
	return &Trial{Agent: Agent{SHA256: sha, Version: "2026.11.01-000000"}, RemoteVia: "lan"}
}

func baseInputs() Inputs {
	return Inputs{DataMounted: true, Build: build, BootID: boot, ImageVersion: "2026.10.29-210857",
		Uptime: 10 * time.Second, TrialDeadline: 3 * time.Minute}
}

func TestChooseRunsTheImageAgentWhenDataIsUnusable(t *testing.T) {
	cases := map[string]func(*Inputs){
		"emergency flag":    func(in *Inputs) { in.Emergency = true },
		"data not mounted":  func(in *Inputs) { in.DataMounted = false },
		"state unreadable":  func(in *Inputs) { in.StateErr = errors.New("bad json") },
		"nothing installed": func(in *Inputs) {},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := baseInputs()
			in.State.Approved = approved("2026.10.30-154553", build)
			if name == "nothing installed" {
				in.State.Approved = nil
			}
			mutate(&in)
			d, _ := Choose(in)
			if d.Role != RoleImage {
				t.Fatalf("want image, got %+v", d)
			}
		})
	}
}

func TestChooseRunsTheTrialFirst(t *testing.T) {
	in := baseInputs()
	in.State.Approved = approved("2026.10.30-154553", build)
	in.State.Trial = trial("new-sha")
	d, _ := Choose(in)
	if d.Role != RoleTrial || d.SHA256 != "new-sha" {
		t.Fatalf("want the trial, got %+v", d)
	}
}

func TestChooseRejectsATrialThatKeepsCrashing(t *testing.T) {
	in := baseInputs()
	in.State.Approved = approved("2026.10.30-154553", build)
	in.State.Trial = trial("new-sha")
	in.State.Failures = []Failure{{SHA256: "new-sha", BootID: boot, Count: TrialFailureLimit}}
	d, st := Choose(in)
	if d.Role != RoleApproved {
		t.Fatalf("want the approved agent after the trial failed, got %+v", d)
	}
	if st.Trial != nil {
		t.Fatal("the failed trial must be cleared")
	}
	if !st.IsRejected("new-sha", build) {
		t.Fatal("the failed trial must be rejected for this build")
	}
}

func TestChooseIgnoresFailuresFromAnotherBoot(t *testing.T) {
	in := baseInputs()
	in.State.Trial = trial("new-sha")
	in.State.Failures = []Failure{{SHA256: "new-sha", BootID: "older-boot", Count: TrialFailureLimit}}
	if d, _ := Choose(in); d.Role != RoleTrial {
		t.Fatalf("failures from another boot must not count, got %+v", d)
	}
}

func TestChooseRunsTheApprovedAgentOnAValidatedBuild(t *testing.T) {
	in := baseInputs()
	in.State.Approved = approved("2026.10.30-154553", "other-build", build)
	d, _ := Choose(in)
	if d.Role != RoleApproved || d.SHA256 != "approved-sha" {
		t.Fatalf("want the approved agent, got %+v", d)
	}
}

func TestChooseFallsBackWhenTheApprovedAgentKeepsCrashing(t *testing.T) {
	in := baseInputs()
	in.State.Approved = approved("2026.10.30-154553", build)
	in.State.Failures = []Failure{{SHA256: "approved-sha", BootID: boot, Count: ApprovedFailureLimit}}
	d, st := Choose(in)
	if d.Role != RoleImage {
		t.Fatalf("want the image agent, got %+v", d)
	}
	if st.Approved == nil {
		t.Fatal("a crashing approved agent is only skipped for this boot, not removed")
	}
}

func TestChooseSkipsAnApprovedAgentRejectedOnThisBuild(t *testing.T) {
	in := baseInputs()
	in.State.Approved = approved("2026.10.30-154553", "other-build")
	in.State.Rejected = []Rejection{{SHA256: "approved-sha", Build: build, Reason: "failed re-validation"}}
	d, st := Choose(in)
	if d.Role != RoleImage {
		t.Fatalf("want the image agent, got %+v", d)
	}
	if st.Approved == nil {
		t.Fatal("the agent stays approved for the builds it passed on")
	}
}

func TestChooseOnANewBuildPrefersANewerOrEqualImageAgent(t *testing.T) {
	for _, imageVersion := range []string{"2026.10.30-154553", "2026.11.02-120000"} {
		in := baseInputs()
		in.ImageVersion = imageVersion
		in.State.Approved = approved("2026.10.30-154553", "old-build")
		d, st := Choose(in)
		if d.Role != RoleImage {
			t.Fatalf("image %s: want the image agent, got %+v", imageVersion, d)
		}
		if st.Approved != nil {
			t.Fatalf("image %s: the older /data agent must be dropped", imageVersion)
		}
	}
}

func TestChooseOnANewBuildRevalidatesANewerDataAgent(t *testing.T) {
	in := baseInputs()
	in.State.Approved = approved("2026.10.30-154553", "old-build")
	d, st := Choose(in)
	if d.Role != RoleTrial || d.SHA256 != "approved-sha" {
		t.Fatalf("want a re-validation trial of the approved agent, got %+v", d)
	}
	if st.Trial == nil || !st.Trial.Revalidation || st.Trial.RemoteVia != "" {
		t.Fatalf("want a local-only re-validation trial, got %+v", st.Trial)
	}
}

func TestChooseAlwaysRevalidatesOnAnUnknownBuild(t *testing.T) {
	in := baseInputs()
	in.Build = ""
	in.State.Approved = approved("2026.10.30-154553", "")
	if d, _ := Choose(in); d.Role != RoleTrial {
		t.Fatalf("an unknown build must never count as validated, got %+v", d)
	}
}

func TestChooseRejectsAFailedRevalidationButKeepsTheAgent(t *testing.T) {
	in := baseInputs()
	in.State.Approved = approved("2026.10.30-154553", "old-build")
	in.State.Trial = &Trial{Agent: *in.State.Approved, Revalidation: true}
	in.State.Failures = []Failure{{SHA256: "approved-sha", BootID: boot, Count: TrialFailureLimit}}
	d, st := Choose(in)
	if d.Role != RoleImage {
		t.Fatalf("want the image agent, got %+v", d)
	}
	if st.Approved == nil || !st.IsRejected("approved-sha", build) {
		t.Fatalf("want the agent kept but rejected for this build, got %+v", st)
	}
}

func TestCountsAsFailure(t *testing.T) {
	cases := []struct {
		name string
		exit Exit
		want bool
	}{
		{"clean exit", Exit{Code: 0, Ran: 5}, false},
		{"quick crash", Exit{Code: 1, Ran: 5}, true},
		{"killed by a signal", Exit{Signaled: true, Ran: 5}, true},
		{"crash after a long run", Exit{Code: 1, Ran: LongRun + 1}, false},
		{"stopped by the supervisor", Exit{Signaled: true, Ran: 5, Stopped: true}, false},
	}
	for _, c := range cases {
		if got := CountsAsFailure(c.exit); got != c.want {
			t.Errorf("%s: CountsAsFailure = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRecordFailureCountsPerBoot(t *testing.T) {
	var st State
	st.RecordFailure("sha", "boot-1")
	st.RecordFailure("sha", "boot-1")
	if got := st.FailureCount("sha", "boot-1"); got != 2 {
		t.Fatalf("want 2 failures, got %d", got)
	}
	st.RecordFailure("sha", "boot-2")
	if got := st.FailureCount("sha", "boot-2"); got != 1 {
		t.Fatalf("a new boot starts from zero, got %d", got)
	}
	if got := st.FailureCount("sha", "boot-1"); got != 0 {
		t.Fatalf("old boots are forgotten, got %d", got)
	}
}

func TestChooseRejectsATrialPastItsDeadline(t *testing.T) {
	in := baseInputs()
	in.State.Trial = trial("new-sha")
	in.State.Trial.Boots = []string{boot}
	in.Uptime = in.TrialDeadline + time.Second
	d, st := Choose(in)
	if d.Role != RoleImage || st.Trial != nil {
		t.Fatalf("a trial past its deadline must be rejected, got %+v", d)
	}
	if r := st.Rejected[0].Reason; !strings.Contains(r, "within") {
		t.Fatalf("reason = %q", r)
	}
}

func TestChooseStartsTheDeadlineAgainOnANewBoot(t *testing.T) {
	in := baseInputs()
	in.State.Trial = trial("new-sha")
	in.State.Trial.Boots = []string{"older-boot"}
	in.State.Trial.StartedAt = time.Hour
	d, st := Choose(in)
	if d.Role != RoleTrial {
		t.Fatalf("want the trial on its second boot, got %+v", d)
	}
	if got := st.Trial.Boots; len(got) != 2 || got[1] != boot || st.Trial.StartedAt != in.Uptime {
		t.Fatalf("trial = %+v", st.Trial)
	}
	if len(in.State.Trial.Boots) != 1 {
		t.Fatal("Choose must not change the state it was given")
	}
}

func TestChooseRejectsATrialThatSpansTooManyBoots(t *testing.T) {
	in := baseInputs()
	in.State.Trial = trial("new-sha")
	in.State.Trial.Boots = []string{"boot-a", "boot-b"}
	d, st := Choose(in)
	if d.Role != RoleImage || !st.IsRejected("new-sha", build) {
		t.Fatalf("a trial that keeps taking the device down must be rejected, got %+v", d)
	}
}

func TestChooseMarksARevalidation(t *testing.T) {
	in := baseInputs()
	in.State.Approved = approved("2026.10.30-154553", "old-build")
	if d, _ := Choose(in); d.Role != RoleTrial || !d.Revalidation {
		t.Fatalf("want a revalidation, got %+v", d)
	}
	in.State.Trial = trial("new-sha")
	if d, _ := Choose(in); d.Revalidation {
		t.Fatal("a new agent's trial is not a revalidation")
	}
}

func TestChooseKeepsCallingARestartedRevalidationARecheck(t *testing.T) {
	in := baseInputs()
	in.State.Approved = approved("2026.10.30-154553", "old-build")
	d, st := Choose(in)
	in.State = st
	if d2, _ := Choose(in); !d2.Revalidation || d2.Reason != d.Reason {
		t.Fatalf("the restarted re-check reads %q, want %q", d2.Reason, d.Reason)
	}
}

func TestChooseForcesTheImageAgent(t *testing.T) {
	in := baseInputs()
	in.ForceImage = "the OS update check did not run"
	in.State.Approved = approved("2026.10.30-154553", build)
	if d, _ := Choose(in); d.Role != RoleImage || d.Reason != in.ForceImage {
		t.Fatalf("want the image agent, got %+v", d)
	}
}

// confirmed is a trial that passed its checks and is in its grace period.
func confirmed(sha string) *Trial {
	t := trial(sha)
	t.Confirmed, t.Build = true, build
	t.Boots, t.StartedAt = []string{boot}, 0
	return t
}

func TestChooseKeepsAConfirmedTrialThroughItsGrace(t *testing.T) {
	in := baseInputs()
	in.State.Approved = approved("2026.10.30-154553", build)
	in.State.Trial = confirmed("new")
	d, st := Choose(in)
	if d.Role != RoleTrial || !d.Grace || st.Trial == nil || st.Approved.SHA256 != "approved-sha" {
		t.Fatalf("during the grace the previous agent stays the fallback: %+v, %+v", d, st)
	}
}

func TestChooseLeavesTheEndOfAGraceToTheRun(t *testing.T) {
	in := baseInputs()
	in.Uptime = 4 * time.Minute
	in.State.Approved = approved("2026.10.30-154553", build)
	in.State.Trial = confirmed("new")
	d, st := Choose(in)
	// Restarted past its deadline: it must answer once more before it is approved.
	if d.Role != RoleTrial || !d.Grace || st.Trial == nil || st.Approved.SHA256 != "approved-sha" {
		t.Fatalf("decision %+v, state %+v", d, st)
	}
}

func TestChooseRejectsAConfirmedTrialThatCrashesOrRestartsTheDevice(t *testing.T) {
	for name, mutate := range map[string]func(*Inputs){
		"crashes": func(in *Inputs) {
			for range TrialFailureLimit {
				in.State.RecordFailure("new", boot)
			}
		},
		"restarts the device": func(in *Inputs) {
			in.State.Trial.Boots = []string{"boot-0", "boot-00"}
			in.BootID = "boot-000"
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := baseInputs()
			in.State.Approved = approved("2026.10.30-154553", build)
			in.State.Trial = confirmed("new")
			mutate(&in)
			d, st := Choose(in)
			if d.SHA256 == "new" || st.Trial != nil || !st.IsRejected("new", build) || st.Approved.SHA256 != "approved-sha" {
				t.Fatalf("a bad agent is still caught in its grace: %+v, %+v", d, st)
			}
		})
	}
}

func TestChooseSettlesAConfirmedTrialOnANewOSBuild(t *testing.T) {
	in := baseInputs()
	in.Build, in.ImageVersion = "build-2", "2026.12.01-000000"
	in.State.Trial = confirmed("new")
	d, st := Choose(in)
	// It passed on build-1; on the new build the usual rule applies: the newer image agent wins.
	if d.Role != RoleImage || st.Trial != nil || st.Approved != nil {
		t.Fatalf("decision %+v, state %+v", d, st)
	}
}

func TestChooseRechecksAnAgentThatOnlyMissedAConfirmOnThisBuild(t *testing.T) {
	in := baseInputs()
	in.State.Trial = &Trial{Agent: Agent{SHA256: "user-sha", Version: "2026.11.05-000000"}, RemoteVia: "relay",
		Confirmed: true, Build: "build-0", Boots: []string{"boot-0"}}
	in.State.RejectUnconfirmed("user-sha", "2026.11.05-000000", build, "the new agent has not reached the cloud yet")
	d, st := Choose(in)
	if d.Role != RoleTrial || !d.Revalidation || d.SHA256 != "user-sha" || d.TrialRejected != "" {
		t.Fatalf("a missed confirm here must not drop the agent approved on another build: %+v, %+v", d, st.Rejected)
	}
}

func TestChooseNamesTheTrialItRejects(t *testing.T) {
	in := baseInputs()
	in.State.Approved = approved("2026.10.30-154553", build)
	in.State.Trial = trial("new-sha")
	in.State.Trial.Boots = []string{boot}
	in.State.Failures = []Failure{{SHA256: "new-sha", BootID: boot, Count: TrialFailureLimit}}
	if d, _ := Choose(in); d.TrialRejected != "failed 3 times during its trial" || d.Role != RoleApproved {
		t.Fatalf("got %+v", d)
	}
	// A trial that ends by promotion is not rejected, even where it failed before.
	in.State.Failures = nil
	in.State.Trial.Confirmed, in.State.Trial.Build = true, "build-0"
	in.State.Reject("new-sha", "2026.11.01-000000", build, "an earlier failure")
	if d, _ := Choose(in); d.TrialRejected != "" {
		t.Fatalf("a promotion is not a rejection: %+v", d)
	}
}

func TestChooseSaysWhyTheApprovedAgentFailedOnThisBuild(t *testing.T) {
	in := baseInputs()
	in.State.Approved = approved("2026.10.30-154553", "build-0")
	in.State.Reject("approved-sha", "2026.10.30-154553", build, "the device restarted 2 times during its trial")
	d, _ := Choose(in)
	if d.Role != RoleImage || d.Reason != "agent 2026.10.30-154553 failed its checks on this OS build: the device restarted 2 times during its trial" {
		t.Fatalf("got %+v", d)
	}
}

func TestChooseKeepsAnApprovedAgentWhoseResendOnlyMissedItsConfirm(t *testing.T) {
	in := baseInputs()
	in.State.Approved = approved("2026.11.01-000000", build)
	in.State.RejectUnconfirmed("approved-sha", "2026.11.01-000000", build, "the client that sent the update has not reached the new agent yet")
	if d, _ := Choose(in); d.Role != RoleApproved {
		t.Fatalf("a missed confirm says nothing against the approved agent, got %+v", d)
	}
	in.State.Approved = approved("2026.11.01-000000", "build-0")
	if d, _ := Choose(in); d.Role != RoleTrial || !d.Revalidation {
		t.Fatalf("not validated here, it is re-checked, got %+v", d)
	}
}

func TestChooseKeepsTheApprovedAgentWhileTheOSUpdateIsUnconfirmed(t *testing.T) {
	in := baseInputs()
	in.ImageVersion = "2026.12.01-000000"
	in.State.Approved = approved("2026.11.01-000000", "build-0")
	in.OSUnconfirmed = true
	if d, st := Choose(in); d.Role != RoleImage || st.Approved == nil {
		t.Fatalf("a revert to build-0 must still find its agent: %+v, approved %+v", d, st.Approved)
	}
	in.OSUnconfirmed = false
	if _, st := Choose(in); st.Approved != nil {
		t.Fatal("once the update is confirmed, the newer image agent replaces it")
	}
}
