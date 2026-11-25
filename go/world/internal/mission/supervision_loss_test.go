package mission

import (
	"testing"
	"time"
)

func TestExplicitSupervisionLossCannotResumeOrReturn(t *testing.T) {
	for _, fallback := range []Action{Hold, Stop, Return} {
		t.Run(string(fallback), func(t *testing.T) {
			e, now, task, authority, c := fixture(t)
			task.Mode = Supervised
			task.Envelope.Fallback = fallback
			authority.TaskDigest = Digest(task)
			if _, err := e.Accept(task, authority); err != nil {
				t.Fatal(err)
			}
			if d, err := e.LoseSupervision(task.ID); err != nil || d.Action != Hold || d.Status.Phase != Accepted {
				t.Fatal(d, err)
			}
			c.SupervisionUntil = *now + 500*time.Millisecond
			if d, err := e.Evaluate(task.ID, c); err != nil || d.Action != Continue {
				t.Fatal(d, err)
			}
			d, err := e.LoseSupervision(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if fallback == Hold {
				if d.Action != Hold || d.Status.Phase != Held {
					t.Fatal(d)
				}
			} else {
				if d.Action != Stop || d.Status.Phase != Failed {
					t.Fatal(d)
				}
			}
			if next, err := e.Evaluate(task.ID, c); err != nil || next.Action == Continue {
				t.Fatal("lease resumed loss", next, err)
			}
		})
	}
}
