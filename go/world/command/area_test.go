package command

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestAreaCLICompleteAtomicHistoricalExport(t *testing.T) {
	c, credentials, row := selectedCLIFixture(t)
	row.Key.Type = "wendy.hazard.v1"
	row.Payload = json.RawMessage(`{"position_m":[1,0,1],"uncertainty_m":0.1}`)
	for i := 0; i < 5; i++ {
		row.Key.Entity = string(rune('a' + i))
		row.Version.Sequence = uint64(i + 1)
		if e := c.Publish(context.Background(), row); e != nil {
			t.Fatal(e)
		}
	}
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	path := filepath.Join(dir, "region.json")
	dest := filepath.Join(dir, "snapshot.json")
	s := selection.Selection{Types: []string{"wendy.hazard.v1"}, Region: &selection.Region{Frame: world.FrameRef{ID: "floor", Revision: "1"}, MinXZ: [2]float64{0, 0}, MaxXZ: [2]float64{2, 2}}}
	raw, _ := json.Marshal(s)
	os.WriteFile(path, raw, 0600)
	args := []string{"fetch", "--name", "West area", "--selection", path, "--credentials", credentials, "--out", dest, "--page-size", "2"}
	var out bytes.Buffer
	if e := runArea(args, &out); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(dest)
	if e != nil {
		t.Fatal(e)
	}
	if e = runArea(args, &out); e == nil {
		t.Fatal("overwrote snapshot")
	}
	after, _ := os.ReadFile(dest)
	if !bytes.Equal(before, after) {
		t.Fatal("changed prior snapshot")
	}
	out.Reset()
	if e = runArea([]string{"show", "--file", dest}, &out); e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(out.Bytes(), []byte("historical complete read")) {
		t.Fatal(out.String())
	}
	// Tampered pages cannot be made a complete cache by preserving old checksum.
	var envelope areaEnvelope
	json.Unmarshal(before, &envelope)
	envelope.Snapshot = bytes.Replace(envelope.Snapshot, []byte(`"complete":false`), []byte(`"complete":true`), 1)
	tampered, _ := json.Marshal(envelope)
	os.WriteFile(dest, tampered, 0600)
	if e = runArea([]string{"show", "--file", dest}, &out); e == nil {
		t.Fatal("accepted changed cached page")
	}
}
