package mission

import (
	"testing"
	"time"
)

func TestReviewStopDuringClockRollbackCannotResume(t *testing.T) {
	e, now, task, a, c := fixture(t)
	mustAccept(t, e, task, a)
	mustEvaluate(t, e, task.ID, c)
	*now = 0
	d, err := e.Stop(stopAuthority(task))
	if err == nil || d.Action != Stop {
		t.Fatalf("expected failed-clock stop decision, got %#v, %v", d, err)
	}
	*now = 2 * time.Second
	d, err = e.Evaluate(task.ID, c)
	if err == nil || d.Action != Stop || d.Status.Phase != Failed {
		t.Fatalf("motion resumed after stop during clock rollback: %#v", d)
	}
}

func TestReviewClockFaultFailsAllTasksAcrossEntryPoints(t *testing.T) {
	for _, trigger := range []string{"accept", "report", "stop", "evaluate"} {
		t.Run(trigger, func(t *testing.T) {
			e, now, task, a, c := fixture(t)
			mustAccept(t, e, task, a)
			mustEvaluate(t, e, task.ID, c)
			second := task
			second.ID = "queued-task"
			mustAccept(t, e, second, authority(second))
			*now = 0
			switch trigger {
			case "accept":
				_, _ = e.Accept(task, a)
			case "report":
				_, _ = e.Report(task.ID, task.Assignee, 3, Succeeded)
			case "stop":
				_, _ = e.Stop(stopAuthority(task))
			case "evaluate":
				_, _ = e.Evaluate(task.ID, c)
			}
			*now = 2 * time.Second
			for _, id := range []string{task.ID, second.ID} {
				d, err := e.Evaluate(id, c)
				if err == nil || d.Action != Stop || d.Status.Phase != Failed {
					t.Fatalf("task %s escaped latched fault: %#v, %v", id, d, err)
				}
			}
			third := task
			third.ID = "delayed-task"
			if _, err := e.Accept(third, authority(third)); err == nil {
				t.Fatal("faulted engine admitted a delayed task")
			}
		})
	}
}
