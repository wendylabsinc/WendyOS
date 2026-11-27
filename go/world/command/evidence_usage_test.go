package command

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
)

func TestEvidenceUsageCLIPrivateReadAuthority(t *testing.T) {
	x := newSiteTestServer(t)
	x.operator.Grant.Grant.Scopes = []string{"evidence-read"}
	var err error
	x.operator.Grant, err = authority.SignGrant(x.operator.Grant.Grant, x.root)
	if err != nil {
		t.Fatal(err)
	}
	writeSiteJSON(t, x.operatorFile, x.operator)
	store, err := evidence.Open(t.TempDir(), evidence.Config{Tenant: "lab", Site: "synthetic-office", ModalityBudgets: map[string]evidence.ModalityBudget{"audio": {QuotaBytes: 1024}}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	x.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.mu.Lock()
		defer x.mu.Unlock()
		x.requests++
		body, _ := io.ReadAll(r.Body)
		raw, e := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		var signed authority.SignedRequest
		if e == nil {
			e = authority.DecodeStrict(raw, &signed)
		}
		if e == nil {
			_, e = x.verifier.Verify(signed, "evidence-read", body)
		}
		if e != nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.URL.Path != "/v1/evidence/usage" || string(body) != "{}" {
			http.Error(w, "wrong usage request", 400)
			return
		}
		u, e := store.Usage()
		if e != nil {
			http.Error(w, e.Error(), 503)
			return
		}
		json.NewEncoder(w).Encode(u)
	})
	var output bytes.Buffer
	args := []string{"usage", "--credentials", x.operatorFile, "--json"}
	if err := runEvidence(args, &output); err != nil {
		t.Fatal(err)
	}
	var u evidence.Usage
	if err = json.Unmarshal(output.Bytes(), &u); err != nil || u.Modalities[0].QuotaBytes != 1024 {
		t.Fatal(output.String(), err)
	}
	x.mu.Lock()
	requests := x.requests
	x.mu.Unlock()
	for _, variant := range []string{"viewer", "public-file", "unknown-flag", "positional", "pin-flag"} {
		t.Run(variant, func(t *testing.T) {
			a := append([]string(nil), args...)
			switch variant {
			case "viewer":
				a[2] = x.viewerFile
			case "public-file":
				os.Chmod(x.operatorFile, 0644)
				defer os.Chmod(x.operatorFile, 0600)
			case "unknown-flag":
				a = append(a, "--unknown")
			case "positional":
				a = append(a, "unexpected")
			case "pin-flag":
				a = append(a, "--request", "pin.json")
			}
			output.Reset()
			if err := runEvidence(a, &output); err == nil || output.Len() != 0 {
				t.Fatal("invalid command accepted", output.String(), err)
			}
			x.mu.Lock()
			defer x.mu.Unlock()
			if x.requests != requests {
				t.Fatal("invalid command reached server")
			}
		})
	}
}
