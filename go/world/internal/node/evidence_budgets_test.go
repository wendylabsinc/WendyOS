package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
)

func TestEvidenceBudgetsAuthenticatedCLIAndPartition(t *testing.T) {
	f := newFixture(t, nil, "capture-a")
	other := newFixture(t, f.root, "capture-b")
	dir := t.TempDir()
	cfg := evidence.Config{Tenant: "tenant", Site: "office", QuotaBytes: 256 << 10, MaxBlobBytes: 128 << 10, MaxRetention: 3 * time.Hour, ModalityBudgets: map[string]evidence.ModalityBudget{"image": {QuotaBytes: 96 << 10, MaxRetentionMS: int64(time.Hour / time.Millisecond)}, "audio": {QuotaBytes: 128 << 10, MaxRetentionMS: int64(2 * time.Hour / time.Millisecond)}, "video": {QuotaBytes: 128 << 10, MaxRetentionMS: 300000}}}
	blobs, err := evidence.Open(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	f.n.cfg.Evidence = blobs
	second, err := evidence.Open(t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	other.n.cfg.Evidence = second
	writer := evidenceCredentials(t, f, "capture-writer", f.audience, "evidence-write")
	reader := evidenceCredentials(t, f, "usage-reader", "", "evidence-read")
	manager := evidenceCredentials(t, f, "investigator", f.audience, "evidence-pin")
	makeClient := func(f *fixture, creds Credentials) *client.Client {
		t.Helper()
		c, e := client.New(client.Config{URL: f.server.URL, Audience: f.audience, Grant: creds.Grant, PrivateKey: creds.PrivateKey, AllowInsecureSimulation: true})
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	cw, cr := makeClient(f, writer), makeClient(f, reader)
	type step struct {
		Phase    string          `json:"phase"`
		Command  string          `json:"command"`
		Success  bool            `json:"success"`
		Response json.RawMessage `json:"response,omitempty"`
		Error    string          `json:"error,omitempty"`
	}
	transcript := struct {
		Scenario string `json:"scenario"`
		Steps    []step `json:"steps"`
	}{Scenario: "Actual built CLI against two authenticated loopback nodes; synthetic bytes labeled by MIME; no physical sensors"}
	binary, output := os.Getenv("WORLD_EVIDENCE_BUDGET_DEMO_BINARY"), os.Getenv("WORLD_EVIDENCE_BUDGET_DEMO_TRANSCRIPT")
	if (binary == "") != (output == "") {
		t.Fatal("both evidence demo environment variables are required")
	}
	invoke := func(phase string, target *fixture, creds Credentials, want bool) evidence.Usage {
		t.Helper()
		var out evidence.Usage
		var raw []byte
		var e error
		if binary != "" {
			p := filepath.Join(t.TempDir(), "credentials.json")
			b, _ := json.Marshal(client.Config{URL: target.server.URL, Audience: target.audience, Grant: creds.Grant, PrivateKey: creds.PrivateKey, AllowInsecureSimulation: true})
			if e = os.WriteFile(p, b, 0600); e != nil {
				t.Fatal(e)
			}
			cmd := exec.Command(binary, "evidence", "usage", "--credentials", p, "--json")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			raw, e = cmd.Output()
			if e != nil {
				e = fmt.Errorf("%v: %s", e, stderr.String())
			}
			if e == nil {
				e = json.Unmarshal(raw, &out)
				if e == nil {
					e = out.Validate()
				}
			}
		} else {
			out, e = makeClient(target, creds).EvidenceUsage(context.Background())
			if e == nil {
				raw, _ = json.Marshal(out)
			}
		}
		if (e == nil) != want {
			t.Fatalf("%s: %v", phase, e)
		}
		x := step{Phase: phase, Command: "world evidence usage --credentials $READER --json", Success: e == nil}
		if e == nil {
			x.Response = raw
		} else {
			x.Error = e.Error()
		}
		transcript.Steps = append(transcript.Steps, x)
		return out
	}
	upload := func(body []byte, kind string) evidence.PutResponse {
		t.Helper()
		p, e := cw.UploadRollingEvidence(context.Background(), body, kind, time.Time{})
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	audio := upload(bytes.Repeat([]byte("a"), 64<<10), "audio/wav")
	first := upload(bytes.Repeat([]byte("1"), 48<<10), "image/png")
	upload(bytes.Repeat([]byte("2"), 48<<10), "image/jpeg")
	before := invoke("Capture A fills its image ceiling", f, reader, true)
	if before.Totals.AllocatedBytes != 160<<10 {
		t.Fatal(before)
	}
	fresh := upload(bytes.Repeat([]byte("3"), 64<<10), "image/png")
	if _, e := cr.GetEvidence(context.Background(), evidence.GetRequest{SHA256: first.SHA256, Length: 1}); e == nil {
		t.Fatal("old image remained")
	}
	if _, e := cr.GetEvidence(context.Background(), evidence.GetRequest{SHA256: audio.SHA256, Length: 1}); e != nil {
		t.Fatal("modality pressure removed audio", e)
	}
	invoke("New image evicts older images; audio retained", f, reader, true)
	cm := makeClient(f, manager)
	if _, e := cm.PinEvidence(context.Background(), evidence.PinRequest{ID: "investigation", SHA256: []string{fresh.SHA256}, RetainUntilUnixMS: time.Now().Add(30 * time.Minute).UnixMilli()}); e != nil {
		t.Fatal(e)
	}
	upload(bytes.Repeat([]byte("4"), 16<<10), "image/png")
	failed := bytes.Repeat([]byte("5"), 40<<10)
	if _, e := cw.UploadRollingEvidence(context.Background(), failed, "image/png", time.Time{}); e == nil {
		t.Fatal("pinned image exceeded cap")
	} else {
		var status *client.HTTPStatusError
		if !errors.As(e, &status) || status.StatusCode != http.StatusInsufficientStorage {
			t.Fatal("quota was not reported as HTTP507", e)
		}
		transcript.Steps = append(transcript.Steps, step{Phase: "Signed upload refused before collateral eviction", Command: "POST /v1/evidence/put (signed evidence-write)", Success: false, Error: e.Error()})
	}
	after := invoke("Quota refusal preserves pinned image, other image and audio", f, reader, true)
	if after.Totals.AllocatedBytes != 144<<10 || after.Totals.PinnedBytes != 64<<10 {
		t.Fatal(after)
	}
	// A second node is independent, and cannot report this node's accounting.
	empty := invoke("Capture B has independent empty storage", other, evidenceCredentials(t, other, "reader-b", "", "evidence-read"), true)
	if empty.Totals.Entries != 0 {
		t.Fatal(empty)
	}
	f.unavailable.Store(true)
	invoke("Capture A partition: usage fails, no cached success", f, reader, false)
	f.unavailable.Store(false)
	invoke("Reconnected: the same retained allocation", f, reader, true)
	partial := bytes.Repeat([]byte("v"), 64<<10)
	hash := sha256.Sum256(partial)
	digest := hex.EncodeToString(hash[:])
	req := evidence.PutRequest{SHA256: digest, Size: int64(len(partial)), Data: partial[:16<<10], ContentType: "video/mp4", RetentionClass: "rolling"}
	receipt, e := cw.PutEvidence(context.Background(), req)
	if e != nil {
		t.Fatal(e)
	}
	u := invoke("Partial video reserves its complete size", f, reader, true)
	if u.Totals.AllocatedBytes != 208<<10 || u.Totals.CommittedBytes != 160<<10 || u.Totals.IncompleteBytes != 64<<10 {
		t.Fatal(u)
	}
	// Reopen the real store while the HTTP listener is stopped, then resume at
	// the durable offset using a fresh request and the original expiry.
	f.server.Close()
	if e = blobs.Close(); e != nil {
		t.Fatal(e)
	}
	blobs, e = evidence.Open(dir, cfg)
	if e != nil {
		t.Fatal(e)
	}
	f.n.cfg.Evidence = blobs
	f.start(t)
	cw, cr = makeClient(f, writer), makeClient(f, reader)
	recovered := invoke("After reopen: reservation and investigation pin survive", f, reader, true)
	if !reflect.DeepEqual(u.Totals, recovered.Totals) {
		t.Fatal("accounting changed on reopen")
	}
	req.Offset = receipt.Received
	req.Data = partial[req.Offset:]
	req.RetainUntilUnixMS = receipt.RetainUntilUnixMS
	complete, e := cw.PutEvidence(context.Background(), req)
	if e != nil || !complete.Complete || complete.RetainUntilUnixMS != receipt.RetainUntilUnixMS {
		t.Fatal(complete, e)
	}
	final := invoke("Video resumes from committed offset without renewed expiry", f, reader, true)
	if final.Totals.IncompleteBytes != 0 || final.Totals.CommittedBytes != 208<<10 {
		t.Fatal(final)
	}
	invoke("Ordinary read authority does not reveal evidence usage", f, f.credentials, false)
	if output != "" {
		raw, _ := json.MarshalIndent(transcript, "", "  ")
		if e = os.WriteFile(output, append(raw, '\n'), 0600); e != nil {
			t.Fatal(e)
		}
	}
}

func TestEvidenceUsageAuthorizationAndSignedBody(t *testing.T) {
	f, blobs := pinHTTPFixture(t)
	storeRolling(t, blobs, "sample", time.Now().Add(time.Hour).UnixMilli())
	reader := evidenceCredentials(t, f, "usage", "", "evidence-read")
	for _, scope := range []string{"read", "publish", "evidence-write", "evidence-pin"} {
		c := evidenceCredentials(t, f, "wrong-"+scope, f.audience, scope)
		if code, _ := call(t, f, c, "POST", "/v1/evidence/usage", scope, []byte(`{}`)); code != 401 {
			t.Fatal(scope, code)
		}
	}
	if code, _ := call(t, f, reader, "POST", "/v1/evidence/usage", "evidence-read", []byte(`{"owner":"other"}`)); code != 400 {
		t.Fatal("unknown selector accepted", code)
	}
	if code, _ := call(t, f, reader, "POST", "/v1/evidence/usage", "evidence-read", []byte(`null`)); code != 400 {
		t.Fatal("null usage request accepted", code)
	}
	req, _ := http.NewRequest("POST", f.server.URL+"/v1/evidence/usage", bytes.NewBufferString(`{}`))
	if err := AuthorizeRequest(req, reader, f.audience, "evidence-read", []byte(`{"changed":true}`)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("unsigned body change accepted")
	}
	wrong := reader
	wrong.Grant.Grant.Site = "other"
	wrong.Grant, err = authority.SignGrant(wrong.Grant.Grant, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, f, wrong, "POST", "/v1/evidence/usage", "evidence-read", []byte(`{}`)); code != 401 {
		t.Fatal("wrong site accepted", code)
	}
	revoked := reader
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli(), GrantIDs: []string{revoked.Grant.Grant.ID}}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, f, revoked, "POST", "/v1/evidence/usage", "evidence-read", []byte(`{}`)); code != 401 {
		t.Fatal("revoked reader accepted", code)
	}
	reader = evidenceCredentials(t, f, "expired-usage", "", "evidence-read")
	reader.Grant.Grant.NotAfterUnixMS = time.Now().Add(-time.Second).UnixMilli()
	reader.Grant.Grant.NotBeforeUnixMS = time.Now().Add(-time.Hour).UnixMilli()
	reader.Grant, err = authority.SignGrant(reader.Grant.Grant, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, f, reader, "POST", "/v1/evidence/usage", "evidence-read", []byte(`{}`)); code != 401 {
		t.Fatal("expired grant accepted", code)
	}
}
