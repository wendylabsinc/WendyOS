package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/production/internal/agentvmfixture"
)

func TestPairCommandExplicitInitAndNoPrematurePeerReceipt(t *testing.T) {
	base, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	dir := filepath.Join(base, "pair")
	var output bytes.Buffer
	args := []string{"init", "--directory", dir, "--state-directory-a", filepath.Join(base, "a"), "--state-directory-b", filepath.Join(base, "b")}
	if e := run(args, &output); e != nil {
		t.Fatal(e)
	}
	var p agentvmfixture.PairPublic
	if e := json.Unmarshal(output.Bytes(), &p); e != nil || p.Kind != agentvmfixture.PairKind || p.Nodes[0].Public.DeviceID == p.Nodes[1].Public.DeviceID {
		t.Fatal("invalid public pair output", e)
	}
	output.Reset()
	if e := run([]string{"peers", "--directory", dir}, &output); e == nil || output.Len() != 0 {
		t.Fatal("unissued pair produced receipt", e)
	}
	if e := run(args, &output); e == nil || output.Len() != 0 {
		t.Fatal("reinitialization succeeded", e)
	}
}

func TestPairCommandRefusesUnknownAndMissingArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}, {"init"}, {"peers"}, {"init", "--directory", "/x", "extra"}, {"peers", "--directory", "/x", "--state-directory-a", "/y"}} {
		var output bytes.Buffer
		if e := run(args, &output); e == nil || output.Len() != 0 {
			t.Fatal("invalid command accepted", args)
		}
	}
}
