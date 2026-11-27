package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/collabdemo"
)

func TestMalformedExclusionLeavesDraftAndSequenceUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scene.json")
	scene := `{"robots":[{"id":"atlas","start":[1,4]}],"grid":{"origin":[0,0],"resolution":0.2,"width":50,"height":40,"blocked":[]},"jobs":[{"id":"inspect","position":[9,4]}]}`
	if err := os.WriteFile(path, []byte(scene), 0600); err != nil {
		t.Fatal(err)
	}
	good := `{"id":"accepted","min":[4,3],"max":[6,5]}`
	line := func(sequence int, exclusion string) string {
		raw, err := json.Marshal(map[string]any{"run_id": "review", "sequence": sequence, "time_s": sequence, "authorized": false, "robots": []map[string]any{{"id": "atlas", "position": []float64{1, 4}, "velocity": []float64{0, 0}, "online": true, "available": true}}, "exclusions": []json.RawMessage{json.RawMessage(exclusion)}})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for _, bad := range []string{
		`{"id":"missing-min","max":[2,2]}`,
		`{"id":"missing-max","min":[-2,-2]}`,
		`{"id":"null","min":null,"max":[2,2]}`,
		`{"id":"null-number","min":[null,0],"max":[2,2]}`,
		`{"id":"malformed","min":[0,0,0],"max":[2,2]}`,
		`{"id":"unknown","min":[0,0],"max":[2,2],"extra":true}`,
	} {
		t.Run(bad, func(t *testing.T) {
			// Reuse the rejected edit's sequence/time. Success proves malformed JSON
			// never reached Tick and therefore did not advance coordinator state.
			input := strings.Join([]string{line(1, good), line(2, bad), line(2, good)}, "\n") + "\n"
			var output bytes.Buffer
			if err := run(path, strings.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(&output)
			want := []collabdemo.Exclusion{{ID: "accepted", Min: collabdemo.Vec{4, 3}, Max: collabdemo.Vec{6, 5}}}
			for _, phase := range []string{"awaiting_authorization", "refused", "awaiting_authorization"} {
				var out collabdemo.Output
				if err := decoder.Decode(&out); err != nil {
					t.Fatal(err)
				}
				if out.Phase != phase || !reflect.DeepEqual(out.Exclusions, want) {
					t.Fatalf("malformed edit changed state: %+v", out)
				}
				for _, robot := range out.Robots {
					if robot.Velocity != (collabdemo.Vec{}) {
						t.Fatal("edit caused motion")
					}
				}
			}
		})
	}
}
