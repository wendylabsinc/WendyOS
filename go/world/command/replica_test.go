package command

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
	"os"
	"path/filepath"
	"testing"
)

func TestReplicaPolicyAndStatusCLI(t *testing.T) {
	dir := t.TempDir()
	selectors := filepath.Join(dir, "selection.json")
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(selectors, []byte(`{"types":["wendy.gas.v0"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"policy", "--tenant", "tenant", "--site", "synthetic-office", "--selection", selectors, "--out", path, "--json"}
	var stdout bytes.Buffer
	if err := runReplica(args, &stdout); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var policy subscription.Policy
	if err = json.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.RequiredTypes) != 1 || policy.RequiredTypes[0] != sitepub.Type {
		t.Fatal("required metadata not visible in generated policy")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("policy mode")
	}
	if err := runReplica(args, &stdout); err == nil {
		t.Fatal("overwrote immutable policy file")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(raw, after) {
		t.Fatal("existing policy changed")
	}
	c, credentials, row := selectedCLIFixture(t, &policy)
	if err := c.Publish(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	row.Key.Type = "wendy.temperature.v0"
	if err := c.Publish(context.Background(), row); err == nil {
		t.Fatal("irrelevant publication succeeded")
	}
	stdout.Reset()
	if err := runReplica([]string{"status", "--credentials", credentials, "--json"}, &stdout); err != nil {
		t.Fatal(err)
	}
	var status subscription.Status
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Mode != "partial" || status.PolicySHA256 != policy.Digest() || status.RetainedRecords != 1 || status.Peers == nil {
		t.Fatalf("incorrect scope/status %+v", status)
	}
	view, err := c.QuerySelected(context.Background(), selection.Request{})
	if err != nil || view.ReplicaScope == nil || !view.ReplicaScope.Partial {
		t.Fatalf("query scope lost: %+v %v", view, err)
	}
}
func TestReplicaPolicyCLIRejectsSpatialOrEmptySelections(t *testing.T) {
	for _, raw := range []string{`{}`, `{"frames":[{"id":"floor","revision":"1"}],"types":["gas"]}`, `{"types":["gas"],"unknown":true}`} {
		dir := t.TempDir()
		path := filepath.Join(dir, "selection")
		out := filepath.Join(dir, "policy")
		os.WriteFile(path, []byte(raw), 0600)
		var stdout bytes.Buffer
		if err := runReplica([]string{"policy", "--tenant", "tenant", "--site", "office", "--selection", path, "--out", out}, &stdout); err == nil {
			t.Fatal("invalid selection accepted")
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatal("invalid policy wrote output")
		}
	}
}
