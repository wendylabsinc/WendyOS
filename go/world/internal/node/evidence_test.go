package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
)

func evidenceCredentials(t *testing.T, f *fixture, principal, owner string, scopes ...string) Credentials {
	t.Helper()
	c := issueCredentials(t, f.root, principal)
	c.Grant.Grant.Scopes = scopes
	c.Grant.Grant.PublishAudience = owner
	g, e := authority.SignGrant(c.Grant.Grant, f.root)
	if e != nil {
		t.Fatal(e)
	}
	c.Grant = g
	return c
}
func TestEvidenceSignedScopeOwnershipAndResume(t *testing.T) {
	f := newFixture(t, nil, "capture-node")
	blobs, e := evidence.Open(t.TempDir(), evidence.Config{Tenant: "tenant", Site: "office"})
	if e != nil {
		t.Fatal(e)
	}
	defer blobs.Close()
	f.n.cfg.Evidence = blobs
	writer := evidenceCredentials(t, f, "camera", "capture-node", "evidence-write")
	reader := evidenceCredentials(t, f, "viewer", "", "evidence-read")
	wrong := evidenceCredentials(t, f, "camera-elsewhere", "other-node", "evidence-write")
	data := bytes.Repeat([]byte("image-bytes"), 10000)
	h := sha256.Sum256(data)
	digest := hex.EncodeToString(h[:])
	until := time.Now().Add(time.Hour)
	put := evidence.PutRequest{SHA256: digest, Size: int64(len(data)), Offset: 0, Data: data[:evidence.MaxChunk], ContentType: "image/png", RetainUntilUnixMS: until.UnixMilli()}
	if code, _ := call(t, f, wrong, "POST", "/v1/evidence/put", "evidence-write", jsonBody(t, put)); code != 401 {
		t.Fatal("wrong audience accepted", code)
	}
	if code, _ := call(t, f, f.credentials, "POST", "/v1/evidence/put", "publish", jsonBody(t, put)); code != 401 {
		t.Fatal("publish grants upload", code)
	}
	code, b := call(t, f, writer, "POST", "/v1/evidence/put", "evidence-write", jsonBody(t, put))
	if code != 200 {
		t.Fatalf("48KiB signedchunk: %d %s", code, b)
	}
	get := evidence.GetRequest{SHA256: digest, Length: 10}
	if code, _ = call(t, f, reader, "POST", "/v1/evidence/get", "evidence-read", jsonBody(t, get)); code != 404 {
		t.Fatal("partial disclosed", code)
	}
	cw, e := client.New(client.Config{URL: f.server.URL, Audience: f.audience, Grant: writer.Grant, PrivateKey: writer.PrivateKey, AllowInsecureSimulation: true})
	if e != nil {
		t.Fatal(e)
	}
	completed, e := cw.UploadEvidence(context.Background(), data, "image/png", until)
	if e != nil || !completed.Complete {
		t.Fatal(completed, e)
	}
	if code, _ = call(t, f, f.credentials, "POST", "/v1/evidence/get", "read", jsonBody(t, get)); code != 401 {
		t.Fatal("ordinary read disclosed evidence", code)
	}
	cr, e := client.New(client.Config{URL: f.server.URL, Audience: f.audience, Grant: reader.Grant, PrivateKey: reader.PrivateKey, AllowInsecureSimulation: true})
	if e != nil {
		t.Fatal(e)
	}
	downloaded, metadata, e := cr.DownloadEvidence(context.Background(), digest, 2<<20)
	if e != nil || metadata.Size != int64(len(data)) || !bytes.Equal(downloaded, data) {
		t.Fatal(metadata, e)
	}
	if _, _, e = cr.DownloadEvidence(context.Background(), digest, 1); e == nil {
		t.Fatal("ignored download size bound")
	}
	// Offset/digest lives in signed body. Changing either invalidates the request.
	body := jsonBody(t, get)
	req, e := http.NewRequest("POST", f.server.URL+"/v1/evidence/get", bytes.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	if e = AuthorizeRequest(req, reader, f.audience, "evidence-read", []byte(`{"sha256":"different","offset":0,"length":10}`)); e != nil {
		t.Fatal(e)
	}
	response, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("unsigned digest accepted")
	}
	var out evidence.PutResponse
	if e = json.Unmarshal(b, &out); e != nil || out.Received != evidence.MaxChunk {
		t.Fatal(out, e)
	}
}
