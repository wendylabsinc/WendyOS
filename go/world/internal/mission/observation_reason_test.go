package mission

import "testing"

func TestObservationReasonsDoNotInventSpatialFindings(t *testing.T) {
	for _, flag := range []string{"invalid", "fault", "outside"} {
		for _, fallback := range []Action{Hold, Stop, Return} {
			t.Run(flag+"-"+string(fallback), func(t *testing.T) {
				e, _, task, _, c := fixture(t)
				task.Envelope.Fallback = fallback
				mustAccept(t, e, task, authority(task))
				mustEvaluate(t, e, task.ID, c)
				c.InsideArea = false
				want := "outside authorized area"
				if flag == "invalid" {
					c.ObservationInvalid = true
					want = "local observations invalid or stale"
				}
				if flag == "fault" {
					c.ObservationInvalid = true
					c.LocalFault = true
					want = "executor reported local fault"
				}
				if fallback == Return {
					want += "; return unavailable"
				}
				d := mustEvaluate(t, e, task.ID, c)
				if d.Status.Reason != want || d.Action == Continue {
					t.Fatalf("wrong failure %+v", d)
				}
				c.InsideArea = true
				c.ObservationInvalid = false
				c.LocalFault = false
				if again := mustEvaluate(t, e, task.ID, c); again != d {
					t.Fatal("negative flag removal resumed latched task")
				}
			})
		}
	}
}

func TestNegativeObservationFlagsPreserveSeparatelyAuthorizedReturn(t *testing.T) {
	e, _, task, _, c := fixture(t)
	task.Envelope.Fallback = Return
	mustAccept(t, e, task, authority(task))
	mustEvaluate(t, e, task.ID, c)
	c.ObservationInvalid = true
	c.LocalFault = true
	c.ReturnSafe = true
	d := mustEvaluate(t, e, task.ID, c)
	if d.Action != Return || d.Status.Phase != Returning || d.Status.Reason != "executor reported local fault" {
		t.Fatal("changed existing separately checked return fallback", d)
	}
	if again := mustEvaluate(t, e, task.ID, c); again != d {
		t.Fatal("changed existing Returning behavior")
	}
	c.ReturnSafe = false
	if d = mustEvaluate(t, e, task.ID, c); d.Action != Stop || d.Status.Phase != Failed {
		t.Fatal("negative flags supplied return authority")
	}
}
