package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
)

func pinHTTPFixture(t *testing.T) (*fixture, *evidence.Store) {
	t.Helper()
	f := newFixture(t, nil, "capture-node")
	store, err := evidence.Open(t.TempDir(), evidence.Config{Tenant: "tenant", Site: "office", QuotaBytes: 128, MaxBlobBytes: 64, MaxEntries: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f.n.cfg.Evidence = store
	return f, store
}

func storeRolling(t *testing.T, store *evidence.Store, text string, until int64) evidence.PutResponse {
	t.Helper()
	data := []byte(text)
	hash := sha256.Sum256(data)
	out, err := store.Put("sensor-owner", evidence.PutRequest{SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data)), Data: data, ContentType: "image/png", RetainUntilUnixMS: until, RetentionClass: "rolling"})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEvidencePinHTTPProtectsCrossOwnerBytesAndReleaseIsFinal(t *testing.T) {
	f, store := pinHTTPFixture(t)
	until := time.Now().Add(time.Hour).UnixMilli()
	first := storeRolling(t, store, "camera-one", until)
	second := storeRolling(t, store, "camera-two", until)
	manager := evidenceCredentials(t, f, "investigator", f.audience, "evidence-pin")
	request := evidence.PinRequest{ID: "case-42", SHA256: []string{first.SHA256}, RetainUntilUnixMS: time.Now().Add(time.Minute).UnixMilli()}
	code, data := call(t, f, manager, "POST", "/v1/evidence/pin", "evidence-pin", jsonBody(t, request))
	var pin evidence.PinResponse
	if code != 200 || json.Unmarshal(data, &pin) != nil || pin.State != "active" || pin.ID != request.ID {
		t.Fatalf("pin: %d %s", code, data)
	}
	other := evidenceCredentials(t, f, "another-investigator", f.audience, "evidence-pin")
	if code, _ = call(t, f, other, "POST", "/v1/evidence/unpin", "evidence-pin", jsonBody(t, evidence.UnpinRequest{ID: request.ID})); code != 404 {
		t.Fatal("another owner released a pin", code)
	}
	third := storeRolling(t, store, "camera-three", until)
	if _, err := store.Get(evidence.GetRequest{SHA256: second.SHA256, Length: 1}); err == nil {
		t.Fatal("unprotected oldest candidate did not leave quota")
	}
	got, err := store.Get(evidence.GetRequest{SHA256: first.SHA256, Length: 1})
	if err != nil || got.RetainUntilUnixMS != first.RetainUntilUnixMS {
		t.Fatal("pin changed metadata or lost cross-owner evidence", got, err)
	}
	c, err := client.New(client.Config{URL: f.server.URL, Audience: f.audience, Grant: manager.Grant, PrivateKey: manager.PrivateKey, AllowInsecureSimulation: true})
	if err != nil {
		t.Fatal(err)
	}
	if released, err := c.UnpinEvidence(context.Background(), evidence.UnpinRequest{ID: request.ID}); err != nil || released.State != "released" {
		t.Fatal(released, err)
	}
	if stale, err := c.PinEvidence(context.Background(), request); err == nil || stale.State != "released" {
		t.Fatal("late create restored released pin", stale, err)
	}
	storeRolling(t, store, "camera-four", until)
	if _, err = store.Get(evidence.GetRequest{SHA256: first.SHA256, Length: 1}); err == nil {
		t.Fatal("released object remained protected")
	}
	if _, err = store.Get(evidence.GetRequest{SHA256: third.SHA256, Length: 1}); err != nil {
		t.Fatal("newer object lost unexpectedly", err)
	}
}

func TestEvidencePinHTTPRejectsScopeDestinationTamperAndReplay(t *testing.T) {
	f, store := pinHTTPFixture(t)
	until := time.Now().Add(time.Hour).UnixMilli()
	object := storeRolling(t, store, "camera", until)
	request := evidence.PinRequest{ID: "case-http", SHA256: []string{object.SHA256}, RetainUntilUnixMS: time.Now().Add(time.Minute).UnixMilli()}
	body := jsonBody(t, request)
	for _, scope := range []string{"read", "evidence-read", "evidence-write"} {
		credential := evidenceCredentials(t, f, "ordinary-"+scope, f.audience, scope)
		for _, path := range []string{"/v1/evidence/pin", "/v1/evidence/unpin"} {
			if code, _ := call(t, f, credential, "POST", path, "evidence-pin", body); code != 401 {
				t.Fatalf("%s authorized %s: %d", scope, path, code)
			}
		}
	}
	wrong := evidenceCredentials(t, f, "wrong-capture", "another-capture-node", "evidence-pin")
	if code, _ := call(t, f, wrong, "POST", "/v1/evidence/pin", "evidence-pin", body); code != 401 {
		t.Fatal("wrong destination accepted", code)
	}
	manager := evidenceCredentials(t, f, "pin-manager", f.audience, "evidence-pin")
	wrongSite := manager
	wrongSite.Grant.Grant.Site = "other-site"
	wrongSite.Grant, _ = authority.SignGrant(wrongSite.Grant.Grant, f.root)
	if code, _ := call(t, f, wrongSite, "POST", "/v1/evidence/pin", "evidence-pin", body); code != 401 {
		t.Fatal("wrong site accepted", code)
	}
	if code, _ := call(t, f, manager, "POST", "/v1/evidence/pin", "evidence-pin", append(bytes.TrimSuffix(body, []byte("}")), []byte(`,"owner":"sensor-owner"}`)...)); code != 400 {
		t.Fatal("body-controlled owner accepted", code)
	}
	signed, err := http.NewRequest("POST", f.server.URL+"/v1/evidence/pin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = AuthorizeRequest(signed, manager, f.audience, "evidence-pin", body); err != nil {
		t.Fatal(err)
	}
	send := func(payload []byte) int {
		req, _ := http.NewRequest("POST", signed.URL.String(), bytes.NewReader(payload))
		req.Header = signed.Header.Clone()
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode
	}
	if code := send([]byte(`{"id":"changed"}`)); code != 401 {
		t.Fatal("changed signed body accepted", code)
	}
	if code := send(body); code != 200 {
		t.Fatal("valid request refused", code)
	}
	if code := send(body); code != 401 {
		t.Fatal("replay accepted", code)
	}
	get := jsonBody(t, evidence.GetRequest{SHA256: object.SHA256, Length: 1})
	if code, _ := call(t, f, manager, "POST", "/v1/evidence/get", "evidence-read", get); code != 401 {
		t.Fatal("pin scope disclosed evidence", code)
	}
}
