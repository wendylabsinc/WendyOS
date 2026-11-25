package rpctrace

import (
	"context"
	"testing"
)

func TestRemoteSnapshotBranchVocabularyIsBoundedAndZeroDuration(t *testing.T) {
	stages := []Stage{StageRemoteCandidate, StageRemoteEpochOmitted, StageRemoteVerifyOmitted, StageRemoteAuthorityOmitted, StageRemoteMembershipOmitted, StageRemoteWindowOmitted, StageRemoteRecordOmitted, StageRemoteVisible}
	names := []string{"remote_candidate", "remote_epoch_omitted", "remote_verify_omitted", "remote_authority_omitted", "remote_membership_omitted", "remote_window_omitted", "remote_record_omitted", "remote_visible"}
	stages = append(stages, StageRemoteRecordBoundOmitted, StageRemotePeerGrantOmitted, StageRemoteSourceAuthorityOmitted, StageRemoteInterpretationOmitted, StageRemoteGrantWindowOmitted, StageRemoteCallbackWindowOmitted)
	names = append(names, "remote_record_bound_omitted", "remote_peer_grant_omitted", "remote_source_authority_omitted", "remote_interpretation_omitted", "remote_grant_window_omitted", "remote_callback_window_omitted")
	ctx := context.Background()
	if n := testing.AllocsPerRun(1000, func() {
		for _, stage := range stages {
			Add(ctx, stage, 0)
		}
	}); n != 0 {
		t.Fatal("disabled branch counts allocate", n)
	}
	s := sink(t, Config{})
	ctx, finish := Begin(ctx, s, labels())
	for _, stage := range stages {
		Add(ctx, stage, 0)
	}
	finish(OK)
	events, _ := drain(t, s)
	if len(events) != 1 || len(events[0].Stages) != len(stages)+1 {
		t.Fatal("missing or unbounded branch vocabulary", events)
	}
	for i, name := range names {
		got := measured(t, events[0], name)
		if stageNames[stages[i]] != name || got.Count != 1 || got.SumNS != 0 || got.MaxNS != 0 {
			t.Fatal("branch was timed or vocabulary changed", name, got)
		}
	}
}
