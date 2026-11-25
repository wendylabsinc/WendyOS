package commands

import (
	"strings"
	"testing"
)

func TestProjectWorldScopesRoundTripAndRejectWildcards(t *testing.T) {
	_, path := projectTestSetup(t, `{"appId":"world-reader","services":{"fusion":{"context":"fusion"}}}`)
	publish := `[{"componentType":"wendy.gas.sample.v1","stream":"gas"}]`
	subscribe := `[{"app":"camera","service":"capture","componentType":"wendy.camera.frame.v1","stream":"frames"}]`
	if _, err := projectTestCommand(t, "add", "world", "--service", "fusion", "--publish", publish, "--subscribe", subscribe, "--file", path); err != nil {
		t.Fatal(err)
	}
	before := readProjectTestFile(t, path)
	for _, want := range []string{`"world"`, `"publish"`, `"subscribe"`, `"wendy.gas.sample.v1"`, `"wendy.camera.frame.v1"`} {
		if !strings.Contains(before, want) {
			t.Fatalf("World scope not retained: %s", want)
		}
	}
	if _, err := projectTestCommand(t, "edit", "world", "--service", "fusion", "--publish", `[{"componentType":"*","stream":"gas"}]`, "--file", path); err == nil {
		t.Fatal("accepted wildcard publication")
	}
	if after := readProjectTestFile(t, path); after != before {
		t.Fatal("invalid edit changed manifest")
	}
}
