package command

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func taskRelayCredentials(t *testing.T) (client.Config, *authority.Verifier, *authority.Verifier) {
	t.Helper()
	root, rootKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	grant, err := authority.SignGrant(authority.Grant{Version: authority.Version, ID: "operator-grant", Principal: "operator", Kind: "operator", Tenant: "tenant", Site: "office", Domain: "simulation", Source: world.Source{Device: "laptop", App: "world", Service: "operator", Stream: "tasks"}, PublicKey: pub, Scopes: []string{"mission", "stop", "read"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	revocations, err := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(59 * time.Minute).UnixMilli()}, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier := func(audience string) *authority.Verifier {
		v, err := authority.NewVerifier(authority.Config{RootPublicKey: root, Tenant: "tenant", Site: "office", Domain: "simulation", Audience: audience, Now: time.Now, Replay: authority.NewMemoryReplay(100)})
		if err != nil {
			t.Fatal(err)
		}
		if err = v.UpdateRevocations(revocations); err != nil {
			t.Fatal(err)
		}
		return v
	}
	return client.Config{Audience: "node-3", Grant: grant, PrivateKey: key, AllowInsecureSimulation: true}, verifier("node-3"), verifier("node-1")
}

func TestTaskCLISeparatesGatewayAndInstructionAudience(t *testing.T) {
	for _, verb := range []string{"send", "stop"} {
		t.Run(verb, func(t *testing.T) {
			cfg, gateway, assignee := taskRelayCredentials(t)
			op := "mission"
			if verb == "stop" {
				op = "stop"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				header, err := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
				if err != nil {
					t.Error(err)
					return
				}
				request, err := authority.DecodeRequest(header)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err = gateway.Verify(request, op, body); err != nil {
					t.Error("gateway request proof", err)
				}
				var instruction authority.SignedInstruction
				if err = authority.DecodeStrict(body, &instruction); err != nil {
					t.Error(err)
					return
				}
				if _, err = assignee.VerifyInstruction(instruction, op); err != nil {
					t.Error("final instruction proof", err)
				}
				if request.Audience != "node-3" || instruction.Audience != "node-1" || r.URL.Path != "/v1/task-relay" || r.Method != http.MethodPost {
					t.Error("wrong relay destination or method")
				}
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"state":"queued","entry":{"audience":"node-1","task_id":"mission"}}`))
			}))
			defer srv.Close()
			cfg.URL = srv.URL
			dir := t.TempDir()
			credentials := filepath.Join(dir, "operator.json")
			data, _ := json.Marshal(cfg)
			if err := os.WriteFile(credentials, data, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"task", verb, "--credentials", credentials, "--destination", "node-1", "--json"}
			if verb == "send" {
				path := filepath.Join(dir, "mission.json")
				data, _ := json.Marshal(tasking.Spec{ID: "mission", NotAfterUnixMS: time.Now().Add(time.Minute).UnixMilli()})
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--spec", path)
			} else {
				args = append(args, "--id", "mission")
			}
			var output bytes.Buffer
			if err := runTask(args, &output); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(output.Bytes(), []byte(`"state":"queued"`)) {
				t.Fatal("queued response lost", output.String())
			}
		})
	}
}

func TestTaskCLIListsRelayedAndDirectStateSeparately(t *testing.T) {
	cfg, _, _ := taskRelayCredentials(t)
	paths := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		_, _ = w.Write([]byte(`{"entries":[],"last_known":true}`))
	}))
	defer srv.Close()
	cfg.URL = srv.URL
	path := filepath.Join(t.TempDir(), "operator.json")
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, relayed := range []bool{false, true} {
		args := []string{"task", "list", "--credentials", path}
		want := "/v1/tasks"
		if relayed {
			args = append(args, "--relayed")
			want = "/v1/task-relay"
		}
		if err := runTask(args, io.Discard); err != nil {
			t.Fatal(err)
		}
		if got := <-paths; got != want {
			t.Fatalf("route %s, want %s", got, want)
		}
	}
	if err := runTask([]string{"task", "send", "--credentials", path, "--relayed"}, io.Discard); err == nil {
		t.Fatal("accepted list flag for send")
	}
	if err := runTask([]string{"task", "list", "--credentials", path, "--destination", "node-1"}, io.Discard); err == nil {
		t.Fatal("accepted destination flag for list")
	}
}
