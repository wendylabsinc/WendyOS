package patroltour

import "testing"

func TestCommissionedObservationHeight(t *testing.T) {
	p := Policy{Method: Method, Stops: []Stop{{RoomID: "room", Position: []float64{1, .3, 1}, Yaw: 0}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, height := range []float64{0, 1.5, .30001} {
		p.Stops[0].Position[1] = height
		if p.Validate() == nil {
			t.Fatal("planar adapter accepted unsupported signed height", height)
		}
	}
}
