package collabdemo

import (
	"encoding/json"
	"testing"
)

func TestExclusionJSONRequiresCompleteFiniteCornersWithoutPartialMutation(t *testing.T) {
	initial := Exclusion{ID: "previous", Min: Vec{4, 3}, Max: Vec{6, 5}}
	for _, raw := range []string{
		`{"id":"missing-min","max":[2,2]}`,
		`{"id":"missing-max","min":[-2,-2]}`,
		`{"id":"missing-both"}`,
		`{"id":"null-min","min":null,"max":[2,2]}`,
		`{"id":"null-max","min":[-2,-2],"max":null}`,
		`{"id":"null-coordinate","min":[null,0],"max":[2,2]}`,
		`{"id":"null-coordinate","min":[0,0],"max":[2,null]}`,
		`{"id":"short","min":[0],"max":[2,2]}`,
		`{"id":"long","min":[0,0],"max":[2,2,3]}`,
		`{"id":"string","min":["0",0],"max":[2,2]}`,
		`{"id":"overflow","min":[0,0],"max":[1e999,2]}`,
		`{"id":"extra","min":[0,0],"max":[2,2],"unknown":true}`,
		`null`, `[]`,
	} {
		t.Run(raw, func(t *testing.T) {
			out := initial
			if err := json.Unmarshal([]byte(raw), &out); err == nil {
				t.Fatal("accepted malformed exclusion")
			}
			if out != initial {
				t.Fatalf("partially changed receiver: %+v", out)
			}
		})
	}
	var out Exclusion
	if err := json.Unmarshal([]byte(`{"id":"explicit-zero","min":[0,0],"max":[2,2]}`), &out); err != nil {
		t.Fatal(err)
	}
	if out != (Exclusion{ID: "explicit-zero", Min: Vec{0, 0}, Max: Vec{2, 2}}) {
		t.Fatal(out)
	}
}
