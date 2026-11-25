package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJSONLinesRefusesMalformedThenAcceptsValidInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scene.json")
	scene := `{"model":{"path":"ignored.xml","sha256":"ignored"},"robots":[{"id":"atlas","start":[0.45,0.45],"body":"allowed-extra"}],"grid":{"origin":[0,0],"resolution":0.3,"width":20,"height":20,"blocked":[]},"jobs":[{"id":"j","label":"inspect","position":[4.05,4.05]}]}`
	if err := os.WriteFile(path, []byte(scene), 0600); err != nil {
		t.Fatal(err)
	}
	line := `{"run_id":"r","sequence":1,"time_s":0,"authorized":true,"stop":false,"robots":[{"id":"atlas","position":[0.45,0.45],"velocity":[0,0],"online":true,"available":true}]}`
	var output bytes.Buffer
	if err := run(path, strings.NewReader("not-json\n"+line+"\n"+line+"\n"), &output); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	for _, phase := range []string{"refused", "running", "refused"} {
		var v map[string]any
		if err := decoder.Decode(&v); err != nil {
			t.Fatal(err)
		}
		if v["phase"] != phase {
			t.Fatal(v)
		}
	}
}
