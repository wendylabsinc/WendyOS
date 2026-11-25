package command

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

func TestProposalCLIReadUsesReadAuthorityAndEscapedTaskID(t *testing.T) {
	cfg, verifier, _ := taskRelayCredentials(t)
	seen := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		proof, err := authority.DecodeRequest(raw)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err = verifier.Verify(proof, "read", nil); err != nil {
			t.Error(err)
			return
		}
		seen++
		switch r.URL.Path {
		case "/v1/tasks/catalog":
			_, _ = w.Write([]byte(`{"assignee":"robot","domain":"simulation","run_id":"test","types":[]}`))
		case "/v1/proposals":
			if r.URL.Query().Get("limit") != "20" || r.URL.Query().Has("after") {
				t.Error("empty cursor sent")
			}
			_, _ = w.Write([]byte(`{"proposals":[],"next":""}`))
		case "/v1/tasks/status":
			if r.URL.Query().Get("task_id") != "id&unexpected=scope" || len(r.URL.Query()) != 1 {
				t.Error("unescaped query")
			}
			http.Error(w, "missing", http.StatusNotFound)
		default:
			t.Error("unexpected write or route", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg.URL = srv.URL
	raw, _ := json.Marshal(cfg)
	path := filepath.Join(t.TempDir(), "reader.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"task", "catalog"}, {"task", "proposal", "list"}} {
		var output bytes.Buffer
		if err := runTask(append(args, "--credentials", path, "--json"), &output); err != nil || output.Len() == 0 {
			t.Fatal(err)
		}
	}
	if err := runTask([]string{"task", "status", "--credentials", path, "--id", "id&unexpected=scope"}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing task accepted")
	}
	if seen != 3 {
		t.Fatal("unexpected calls", seen)
	}
	for _, args := range [][]string{{"task", "proposal", "list", "--limit", "101"}, {"task", "proposal", "get", "--id", "../tasks"}, {"task", "catalog", "--destination", "elsewhere"}, {"task", "proposal", "approve"}} {
		if err := runTask(append(args, "--credentials", path), &bytes.Buffer{}); err == nil {
			t.Fatal("invalid review operation accepted", args)
		}
	}
	if seen != 3 {
		t.Fatal("invalid flags reached gateway")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := runTask([]string{"task", "catalog", "--credentials", path}, &bytes.Buffer{}); err == nil {
		t.Fatal("public credential file accepted")
	}
}

func TestProposalCLIOriginSelectsReadOnlyInbox(t *testing.T) {
	cfg, verifier, _ := taskRelayCredentials(t)
	seen := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		proof, e := authority.DecodeRequest(raw)
		if e != nil {
			t.Error(e)
			return
		}
		if _, e = verifier.Verify(proof, "read", nil); e != nil {
			t.Error(e)
			return
		}
		if r.Method != "GET" || r.URL.Path != "/v1/proposal-inbox" || r.URL.Query().Get("origin") != "origin-a" {
			t.Error("wrong remote review route", r.URL)
		}
		seen++
		if r.URL.Query().Get("id") != "" {
			w.Write([]byte(`{"origin":"origin-a","record":{"proposal":{"id":"one"}},"historical_provenance_verified":true,"executable":false}`))
		} else {
			w.Write([]byte(`{"origin":"origin-a","proposals":[],"next":""}`))
		}
	}))
	defer srv.Close()
	cfg.URL = srv.URL
	b, _ := json.Marshal(cfg)
	path := filepath.Join(t.TempDir(), "reader.json")
	if e := os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{{"task", "proposal", "list", "--origin", "origin-a"}, {"task", "proposal", "get", "--origin", "origin-a", "--id", "one"}} {
		if e := runTask(append(args, "--credentials", path, "--json"), &bytes.Buffer{}); e != nil {
			t.Fatal(e)
		}
	}
	if seen != 2 {
		t.Fatal(seen)
	}
	for _, args := range [][]string{{"task", "proposal", "list", "--origin", "bad&scope=x"}, {"task", "proposal", "get", "--origin", "origin-a", "--id", "../tasks"}, {"task", "propose", "--origin", "origin-a"}} {
		if e := runTask(append(args, "--credentials", path), &bytes.Buffer{}); e == nil {
			t.Fatal("invalid remote operation accepted", args)
		}
	}
	if seen != 2 {
		t.Fatal("invalid scope reached server", seen)
	}
}
