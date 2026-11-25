package missioncontrol

import (
	"encoding/json"
	"testing"
)

func TestTypedInspectTargetRejectsAmbiguousAndExecutableParameters(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"target":null}`, `{"target":[1,2]}`, `{"target":[1,2,3,4]}`, `{"target":[1,null,3]}`, `{"target":[1,"2",3]}`, `{"target":[1,2,3],"instruction":"move"}`, `{"target":[1,2,3],"target":[4,5,6]}`, `{"target":[1,2,3]} {}`} {
		if _, err := DecodeTarget([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if p, err := DecodeTarget([]byte(`{"target":[1,0.2,0]}`)); err != nil || p != [3]float64{1, .2, 0} {
		t.Fatal(p, err)
	}
}
func TestBoundsNeverNormalizeNullOrShortVectors(t *testing.T) {
	for _, raw := range []string{`{"min":[0,0],"max":[1,1,1]}`, `{"min":null,"max":[1,1,1]}`, `{"min":[0,null,0],"max":[1,1,1]}`, `{"min":[0,0,0,0],"max":[1,1,1]}`} {
		var b Bounds
		if json.Unmarshal([]byte(raw), &b) == nil {
			t.Fatal(raw)
		}
	}
}
