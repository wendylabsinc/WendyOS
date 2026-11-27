package serviceaccess

import "testing"

func TestCursorPreservesExactKeyAndCannotInjectScope(t *testing.T) {
	s := Source{App: "com.example.camera", Service: "north", ComponentType: "wendy.sensor.capture.v1", Stream: "north/people,raw=x"}
	key := Key(s, "sensor/原本")
	decoded, e := DecodeCursor(Cursor(key))
	if e != nil || decoded != key {
		t.Fatal("cursor changed entity", e)
	}
	for _, v := range []string{"", Cursor(`[]`), Cursor(`["com.example.camera","north","*","north","entity"]`), Cursor(key + " "), Cursor(`null`)} {
		if _, e := DecodeCursor(v); e == nil {
			t.Fatal("invalid cursor accepted", v)
		}
	}
}
