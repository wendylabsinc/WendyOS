package rpctrace

import (
	"context"
	"testing"
	"time"
)

func TestArchiveStageVocabularyAndNilNoop(t *testing.T) {
	stages := []Stage{StageArchivePrune, StageArchiveRemember, StageArchiveUsage, StageArchiveVerify, StageArchiveCommit}
	names := []string{"archive_prune", "archive_remember", "archive_usage", "archive_verify", "archive_commit"}
	ctx := context.Background()
	if n := testing.AllocsPerRun(1000, func() {
		for _, stage := range stages {
			Start(ctx, stage)()
			Add(ctx, stage, time.Nanosecond)
		}
	}); n != 0 {
		t.Fatal("disabled archive spans allocate", n)
	}
	s := sink(t, Config{})
	ctx, finish := Begin(ctx, s, labels())
	for _, stage := range stages {
		stop := Start(ctx, stage)
		stop()
		stop()
	}
	finish(OK)
	events, _ := drain(t, s)
	if len(events) != 1 || len(events[0].Stages) != len(stages)+1 {
		t.Fatal("unbounded or missing archive labels", events)
	}
	for i, name := range names {
		if stageNames[stages[i]] != name || measured(t, events[0], name).Count != 1 {
			t.Fatal("archive vocabulary drift", name)
		}
	}
}
