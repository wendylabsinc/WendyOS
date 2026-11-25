package command

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/node"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func selectedCLIFixture(t *testing.T, policies ...*subscription.Policy) (*client.Client, string, world.Component) {
	t.Helper()
	dir := t.TempDir()
	root, rk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, pk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	source := world.Source{Device: "synthetic-office", App: "sensors", Service: "publisher", Stream: "readings"}
	grant, err := authority.SignGrant(authority.Grant{Version: 1, ID: "selected-cli", PublishAudience: "selected-cli", Tenant: "tenant", Site: "synthetic-office", Domain: "simulation", Principal: "selected-cli", Kind: "app", Source: source, PublicKey: pub, Scopes: []string{"read", "publish"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, rk)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := authority.NewVerifier(authority.Config{RootPublicKey: root, Tenant: "tenant", Site: "synthetic-office", Domain: "simulation", Audience: "selected-cli", Now: time.Now, Replay: authority.NewMemoryReplay(1000)})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "synthetic-office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli()}, rk)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifier.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	var policy *subscription.Policy
	if len(policies) > 0 {
		policy = policies[0]
	}
	s, err := store.OpenSubscribed(filepath.Join(dir, "store.json"), world.Config{Tenant: "tenant", Site: "synthetic-office", ClockDomain: "simulation:cli", Now: time.Now, MaxEntries: 256, MaxFutureSkew: time.Second, AllowSynthetic: true, Frames: []world.FrameRef{{ID: "floor", Revision: "1"}}}, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	n, err := node.New(node.Config{Tenant: "tenant", Site: "synthetic-office", ProofPath: filepath.Join(dir, "proofs.json"), MaxRecords: 256}, s, verifier)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(n.Handler())
	t.Cleanup(server.Close)
	cfg := client.Config{URL: server.URL, Audience: "selected-cli", Grant: grant, PrivateKey: pk, AllowInsecureSimulation: true}
	c, err := client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "reader.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	epoch, err := c.NextEpoch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	component := world.Component{Key: world.Key{Tenant: "tenant", Site: "synthetic-office", Entity: "sensor-000", Type: "wendy.gas.v0", Source: source}, Version: world.Version{Epoch: epoch, Sequence: 1}, Frame: world.FrameRef{ID: "floor", Revision: "1"}, Timing: world.Timing{SampleTime: time.Now(), ClockDomain: "simulation:cli", ErrorKnown: true}, TTL: 10 * time.Minute, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "selected-cli-test", Simulation: &world.SimulationContext{RunID: "cli", ScenarioVersion: "1", ClockDomain: "simulation:cli"}}, Payload: json.RawMessage(`{"concentration":20,"unit":"ppm"}`)}
	return c, path, component
}

func TestSelectedReadCLIRealNode(t *testing.T) {
	c, credentials, template := selectedCLIFixture(t)
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		row := template
		row.Key.Entity = fmt.Sprintf("sensor-%03d", i)
		row.Version.Sequence = uint64(i + 1)
		if i%2 == 1 {
			row.Key.Type = "wendy.temperature.v0"
		}
		if err := c.Publish(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	selectPath := filepath.Join(t.TempDir(), "selection.json")
	if err := os.WriteFile(selectPath, []byte(`{"types":["wendy.gas.v0"],"frames":[{"id":"floor","revision":"1"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	type step struct {
		Command  string          `json:"command"`
		ExitCode int             `json:"exit_code"`
		Summary  string          `json:"summary"`
		Response json.RawMessage `json:"response,omitempty"`
	}
	transcript := struct {
		Scenario  string `json:"scenario"`
		Simulated bool   `json:"simulated"`
		Steps     []step `json:"steps"`
	}{Scenario: "Actual CLI against a local authenticated node with 200 synthetic sensor records", Simulated: true}
	invoke := func(args []string, description string, wantSuccess bool) selection.Response {
		t.Helper()
		var stdout, stderr bytes.Buffer
		var err error
		if binary := os.Getenv("WORLD_SELECTED_DEMO_BINARY"); binary != "" {
			command := exec.Command(binary, worldCLIArgs(args)...)
			command.Stdout = &stdout
			command.Stderr = &stderr
			err = command.Run()
		} else {
			err = runSelectedRead(args, &stdout)
			if err != nil {
				fmt.Fprint(&stderr, err)
			}
		}
		if (err == nil) != wantSuccess {
			t.Fatalf("%s: %v %s", description, err, stderr.String())
		}
		out := selection.Response{}
		code := 0
		summary := stderr.String()
		if err == nil {
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			summary = fmt.Sprintf("records=%d complete=%t continuation=%t timed_out=%t", len(out.Components), out.Complete, out.NextCursor != "", out.TimedOut)
		} else {
			code = 1
		}
		transcript.Steps = append(transcript.Steps, step{description, code, summary, json.RawMessage(bytes.Clone(stdout.Bytes()))})
		return out
	}
	base := []string{"query", "--credentials", credentials, "--selection", selectPath, "--page-size", "17", "--json"}
	first := invoke(base, "world query --credentials $READER --selection gas.json --page-size 17 --json", true)
	if first.Complete || len(first.Components) != 17 {
		t.Fatal("first page bounds")
	}
	seen := map[string]bool{}
	page := first
	pages := 0
	for {
		pages++
		for _, row := range page.Components {
			if seen[row.Key.Entity] || row.Key.Type != "wendy.gas.v0" {
				t.Fatal("duplicate or unselected CLI row")
			}
			seen[row.Key.Entity] = true
		}
		if page.Complete {
			break
		}
		page = invoke(append(append([]string{}, base...), "--cursor", page.NextCursor), "world query --credentials $READER --selection gas.json --page-size 17 --cursor $NEXT --json", true)
	}
	if len(seen) != 100 || pages != 6 {
		t.Fatalf("coverage: %d records, %d pages", len(seen), pages)
	}
	watch := []string{"watch", "--credentials", credentials, "--selection", selectPath, "--revision", first.Revision, "--timeout-ms", "100", "--json"}
	out := invoke(watch, "world watch --credentials $READER --selection gas.json --revision $REVISION --timeout-ms 100 --json", true)
	if !out.TimedOut || out.Revision != first.Revision {
		t.Fatal("unchanged watch")
	}
	update := template
	update.Version.Sequence = 300
	update.Payload = json.RawMessage(`{"concentration":75,"unit":"ppm"}`)
	update.Timing.SampleTime = time.Now()
	if err := c.Publish(ctx, update); err != nil {
		t.Fatal(err)
	}
	out = invoke(watch, "world watch --credentials $READER --selection gas.json --revision $REVISION --timeout-ms 100 --json  # after signed gas update", true)
	if out.TimedOut || out.Revision == first.Revision {
		t.Fatal("selected update not reported")
	}
	invoke(append(append([]string{}, base...), "--cursor", first.NextCursor), "world query --credentials $READER --selection gas.json --cursor $OLD_NEXT --json", false)
	invoke(base, "world query --credentials $READER --selection gas.json --page-size 17 --json  # restart after changed view", true)
	if path := os.Getenv("WORLD_SELECTED_DEMO_TRANSCRIPT"); path != "" {
		raw, err := json.MarshalIndent(transcript, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSelectedCLIRejectsMalformedInputBeforeNetwork(t *testing.T) {
	for _, args := range [][]string{{"query"}, {"query", "--credentials", "missing", "--revision", "x"}, {"query", "--credentials", "missing", "--timeout-ms", "0"}, {"watch", "--credentials", "missing", "--cursor", "x"}, {"watch", "--credentials", "missing", "--timeout-ms", "5001"}, {"query", "--credentials", "missing", "--page-size", "129"}} {
		var out bytes.Buffer
		if err := runSelectedRead(args, &out); err == nil || out.Len() != 0 {
			t.Fatalf("accepted: %v", args)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "reader")
	if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := queryReadFile(path, 1024, true); err == nil {
		t.Fatal("public credentials")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := queryReadFile(link, 1024, true); err == nil {
		t.Fatal("credential symlink")
	}
	if _, err := queryReadFile(dir, 1024, false); err == nil {
		t.Fatal("directory")
	}
	if _, err := queryReadFile(path, 1, false); err == nil {
		t.Fatal("oversized config")
	}
	selectors := filepath.Join(dir, "selection")
	os.WriteFile(selectors, []byte(`{"region":"any"}`), 0600)
	var out bytes.Buffer
	err := runSelectedRead([]string{"query", "--credentials", path, "--selection", selectors}, &out)
	if err == nil || !strings.Contains(err.Error(), "selection:") {
		t.Fatalf("unknown selector: %v", err)
	}
}
