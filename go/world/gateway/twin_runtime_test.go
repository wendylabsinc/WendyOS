package gateway_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twin"
	"github.com/wendylabsinc/wendy/go/world/gateway"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type runtimeFixture struct {
	path        string
	credentials client.Config
}

func fixture(t *testing.T, listen string) runtimeFixture {
	t.Helper()
	root, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	grant, err := authority.SignGrant(authority.Grant{Version: 1, ID: "combined-runtime", Tenant: "tenant", Site: "office", Domain: "simulation", Principal: "capture", Kind: "app", PublishAudience: "runtime", PublicKey: pub, Source: world.Source{Device: "synthetic", App: "test", Service: "capture", Stream: "sensor"}, Scopes: []string{"read", "publish"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, key)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli()}, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	raw, err := json.Marshal(map[string]any{"listen": listen, "data_path": filepath.Join(dir, "state.json"), "tenant": "tenant", "site": "office", "clock_domain": "simulation:combined", "audience": "runtime", "root_public_key": root, "revocations": rev, "frames": []world.FrameRef{{ID: "floor", Revision: "1"}}, "allow_insecure_simulation": true})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return runtimeFixture{path, client.Config{Audience: "runtime", Grant: grant, PrivateKey: private, AllowInsecureSimulation: true}}
}

func (f runtimeFixture) client(t *testing.T, address string) *client.Client {
	t.Helper()
	cfg := f.credentials
	cfg.URL = "http://" + address
	c, err := client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func listener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}
func twinServer(t *testing.T) *twin.Server {
	t.Helper()
	s, err := twin.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &twin.Server{Store: s}
}
func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("combined service did not close")
		return nil
	}
}
func status(t *testing.T, url string) int {
	t.Helper()
	c := &http.Client{Timeout: 2 * time.Second}
	r, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	_, _ = io.Copy(io.Discard, r.Body)
	return r.StatusCode
}

func TestCombinedTwinServesSignedWorldAndPreservesState(t *testing.T) {
	f := fixture(t, "127.0.0.1:0")
	ln := listener(t)
	address := ln.Addr().String()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	server := twinServer(t)
	go func() {
		done <- server.ServeWithWorld(ctx, ln, twin.WorldOptions{ConfigPath: f.path, Ready: func(a string) { ready <- a }})
	}()
	var worldAddress string
	select {
	case worldAddress = <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("World startup not ready")
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			cancel()
			_ = waitDone(t, done)
		}
	})
	c := f.client(t, worldAddress)
	epoch, err := c.NextEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	component := world.Component{Key: world.Key{Tenant: "tenant", Site: "office", Entity: "gas", Type: "wendy.gas.v0", Source: f.credentials.Grant.Grant.Source}, Version: world.Version{Epoch: epoch, Sequence: 1}, Frame: world.FrameRef{ID: "floor", Revision: "1"}, Timing: world.Timing{SampleTime: time.Now(), ClockDomain: "simulation:combined", ErrorKnown: true}, TTL: time.Minute, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "test", Simulation: &world.SimulationContext{RunID: "combined", ScenarioVersion: "1", ClockDomain: "simulation:combined"}}, Payload: json.RawMessage(`{"concentration":20,"unit":"ppm"}`)}
	if err = c.Publish(ctx, component); err != nil {
		t.Fatal(err)
	}
	snapshot, err := c.Query(ctx)
	if err != nil || len(snapshot.Components) != 1 {
		t.Fatalf("signed query: %+v %v", snapshot, err)
	}
	if got := status(t, "http://"+address+"/v1/rooms"); got != 200 {
		t.Fatalf("existing twin rooms: %d", got)
	}
	if got := status(t, "http://"+worldAddress+"/v1/components"); got != 401 && got != 403 {
		t.Fatalf("unsigned World read accepted: %d", got)
	}
	if got := status(t, "http://"+address+"/v1/components"); got != 404 {
		t.Fatalf("World authority leaked into twin routes: %d", got)
	}
	wrong := f.credentials
	wrong.URL = "http://" + worldAddress
	wrong.Audience = "another-node"
	wrongClient, err := client.New(wrong)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wrongClient.Query(ctx); err == nil {
		t.Fatal("wrong signed audience accepted")
	}
	cancel()
	err = waitDone(t, done)
	stopped = true
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{address, worldAddress} {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatalf("listener survived shared cancellation: %s", addr)
		}
	}
	restarted, err := gateway.Start(t.Context(), f.path)
	if err != nil {
		t.Fatal("state locks survived shutdown", err)
	}
	defer restarted.Close()
	snapshot, err = f.client(t, restarted.Address()).Query(t.Context())
	if err != nil || len(snapshot.Components) != 1 || snapshot.Components[0].Provenance.Origin != world.Simulated {
		t.Fatalf("restart lost record/provenance: %+v %v", snapshot, err)
	}
}

func TestCombinedTwinPartialStartupReleasesOwnedResources(t *testing.T) {
	for _, which := range []string{"world-listener", "twin-listener"} {
		t.Run(which, func(t *testing.T) {
			occupied := listener(t)
			defer occupied.Close()
			f := fixture(t, occupied.Addr().String())
			ln := listener(t)
			twinAddress := ln.Addr().String()
			if which == "twin-listener" {
				occupied.Close()
				ln.Close()
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var networkError *net.OpError
			if err := twinServer(t).ServeWithWorld(ctx, ln, twin.WorldOptions{ConfigPath: f.path}); err == nil || !errors.As(err, &networkError) {
				t.Fatalf("expected listener failure after state opened, got %v", err)
			}
			conn, err := net.DialTimeout("tcp", twinAddress, 100*time.Millisecond)
			if err == nil {
				conn.Close()
				t.Fatal("failed startup kept twin listener")
			}
			occupied.Close()
			restarted, err := gateway.Start(ctx, f.path)
			if err != nil {
				t.Fatal("partial startup retained World listener or locks", err)
			}
			if _, err = f.client(t, restarted.Address()).Query(ctx); err != nil {
				t.Fatal(err)
			}
			if err = restarted.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCombinedTwinUsesNestedWorldDirectoryWithoutReplacingRooms(t *testing.T) {
	for _, collision := range []bool{false, true} {
		t.Run(map[bool]string{false: "nested-world", true: "room-collision"}[collision], func(t *testing.T) {
			f := fixture(t, "127.0.0.1:0")
			root := t.TempDir()
			worldDirectory := filepath.Join(root, "world")
			if err := os.Mkdir(worldDirectory, 0700); err != nil {
				t.Fatal(err)
			}
			roomPath := filepath.Join(root, "11111111-1111-4111-8111-111111111111.json")
			original := []byte(`{"existing":"room"}`)
			if err := os.WriteFile(roomPath, original, 0600); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			var cfg map[string]any
			if err = json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			cfg["data_path"] = filepath.Join(worldDirectory, "state.json")
			if collision {
				cfg["data_path"] = roomPath
			}
			raw, err = json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			f.path = filepath.Join(worldDirectory, "config.json")
			if err = os.WriteFile(f.path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			store, err := twin.NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			service := &twin.Server{Store: store}
			ln := listener(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			ready := make(chan string, 1)
			go func() {
				done <- service.ServeWithWorld(ctx, ln, twin.WorldOptions{ConfigPath: f.path, Ready: func(s string) { ready <- s }})
			}()
			if collision {
				if err := waitDone(t, done); err == nil {
					t.Fatal("World accepted twin room as its state")
				}
			} else {
				select {
				case addr := <-ready:
					if _, err = f.client(t, addr).Query(ctx); err != nil {
						cancel()
						_ = waitDone(t, done)
						t.Fatal(err)
					}
				case err := <-done:
					t.Fatal(err)
				case <-time.After(5 * time.Second):
					t.Fatal("nested World not ready")
				}
				cancel()
				if err := waitDone(t, done); err != nil {
					t.Fatal(err)
				}
			}
			got, err := os.ReadFile(roomPath)
			if err != nil || string(got) != string(original) {
				t.Fatalf("World changed existing room: %q %v", got, err)
			}
		})
	}
}
