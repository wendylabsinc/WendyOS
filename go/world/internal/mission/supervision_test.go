package mission

import (
	"testing"
	"time"
)

func TestSupervisedInitialWaitDoesNotTriggerFallback(t *testing.T) {
	for _, fallback := range []Action{Hold, Return, Stop} {
		t.Run(string(fallback), func(t *testing.T) {
			e, now, task, _, c := fixture(t)
			task.Mode = Supervised
			task.Envelope.Fallback = fallback
			c.ReturnSafe = true
			mustAccept(t, e, task, authority(task))
			for _, until := range []time.Duration{0, *now, *now + 2*time.Second} {
				c.SupervisionUntil = until
				d := mustEvaluate(t, e, task.ID, c)
				if d.Action != Hold || d.Status.Phase != Accepted {
					t.Fatalf("initial wait invoked fallback: %+v", d)
				}
			}
			c.SupervisionUntil = *now + 500*time.Millisecond
			if d := mustEvaluate(t, e, task.ID, c); d.Action != Continue || d.Status.Phase != Running {
				t.Fatal(d)
			}
			*now += 500 * time.Millisecond
			d := mustEvaluate(t, e, task.ID, c)
			if d.Action != fallback || d.Status.Phase == Accepted {
				t.Fatalf("running task did not follow fallback: %+v", d)
			}
			c.SupervisionUntil = *now + 500*time.Millisecond
			if after := mustEvaluate(t, e, task.ID, c); after.Action == Continue {
				t.Fatalf("lost supervision auto-resumed: %+v", after)
			}
		})
	}
}
func TestInitialSupervisionWaitDoesNotReserveActuator(t *testing.T) {
	e, now, waiting, _, c := fixture(t)
	waiting.Mode = Supervised
	mustAccept(t, e, waiting, authority(waiting))
	if d := mustEvaluate(t, e, waiting.ID, c); d.Status.Phase != Accepted {
		t.Fatal(d)
	}
	other := waiting
	other.ID = "ready-mission"
	other.Mode = Autonomous
	mustAccept(t, e, other, authority(other))
	if d := mustEvaluate(t, e, other.ID, c); d.Action != Continue {
		t.Fatal(d)
	}
	c.SupervisionUntil = *now + 500*time.Millisecond
	if d, err := e.Evaluate(waiting.ID, c); err == nil || d.Action != Hold || d.Status.Phase != Accepted {
		t.Fatalf("waiting task stole actuator: %+v %v", d, err)
	}
}
func TestInitialSupervisionWaitHonorsExpiryStopAndRestart(t *testing.T) {
	for _, mode := range []string{"deadline", "grant", "stop", "restart"} {
		t.Run(mode, func(t *testing.T) {
			e, now, task, _, c := fixture(t)
			task.Mode = Supervised
			a := authority(task)
			if mode == "deadline" {
				task.Deadline = *now + time.Second
				a = authority(task)
			}
			if mode == "grant" {
				a.ValidUntil = *now + time.Second
			}
			mustAccept(t, e, task, a)
			mustEvaluate(t, e, task.ID, c)
			switch mode {
			case "deadline", "grant":
				*now += time.Second
			case "stop":
				if _, err := e.Stop(stopAuthority(task)); err != nil {
					t.Fatal(err)
				}
			case "restart":
				e.Restart()
			}
			c.SupervisionUntil = *now + 500*time.Millisecond
			d := mustEvaluate(t, e, task.ID, c)
			if d.Action != Stop || !d.Status.Phase.Terminal() {
				t.Fatalf("waiting task escaped %s: %+v", mode, d)
			}
		})
	}
}
