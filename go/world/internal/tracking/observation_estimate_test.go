package tracking

import (
	"reflect"
	"testing"
	"time"
)

func TestObservationEstimate(t *testing.T) {
	tr := tracker(t)
	step(t, tr, 0, detection("a", 1, 0, 0, 0, 0))
	step(t, tr, .2, detection("a", 2, 0, .2, .3, 0))
	d := detection("a", 3, 0, .4, .6, 0)
	out := step(t, tr, .9, d)
	if len(out) != 1 {
		t.Fatal(out)
	}
	a := out[0]
	if a.ObservedPositionXZ == a.PositionXZ || a.ObservedCovariance[0] >= a.Covariance[0] || !a.LastObserved.Equal(d.SampleTime) || !a.Time.Equal(start.Add(900*time.Millisecond)) {
		t.Fatalf("capture and prediction not separate: %+v", a)
	}
	b := step(t, tr, 1, d)[0]
	if b.State != "coasting" || b.ObservedPositionXZ != a.ObservedPositionXZ || b.ObservedCovariance != a.ObservedCovariance || !b.LastObserved.Equal(a.LastObserved) || !b.ValidUntil.Equal(a.ValidUntil) || !reflect.DeepEqual(b.Members, a.Members) {
		t.Fatalf("duplicate rewrote observation: %+v", b)
	}
}
