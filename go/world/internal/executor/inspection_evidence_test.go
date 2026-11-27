package executor

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

type inspectionEvidenceHTTP struct {
	store      *evidence.Store
	mu         sync.Mutex
	puts       []evidence.PutRequest
	gets       int
	denyUpload int
}

func (f *inspectionEvidenceHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/v1/evidence/put":
		var request evidence.PutRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid", 400)
			return
		}
		f.puts = append(f.puts, request)
		if f.denyUpload != 0 {
			http.Error(w, "denied", f.denyUpload)
			return
		}
		response, err := f.store.Put("executor", request)
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		json.NewEncoder(w).Encode(response)
	case "/v1/evidence/get":
		f.gets++
		var request evidence.GetRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid", 400)
			return
		}
		response, err := f.store.Get(request)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		json.NewEncoder(w).Encode(response)
	default:
		http.NotFound(w, r)
	}
}
func inspectionEvidenceFixture(t *testing.T) (NodeGateway, *inspectionEvidenceHTTP) {
	t.Helper()
	store, err := evidence.Open(t.TempDir(), evidence.Config{Tenant: "test", Site: "office", MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	fixture := &inspectionEvidenceHTTP{store: store}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// Request signing is exercised here; authority admission has its own node
	// tests. This endpoint isolates upload policy and the real evidence store.
	c, err := client.New(client.Config{URL: server.URL, Audience: "node", AllowInsecureSimulation: true, PrivateKey: key, Grant: authority.SignedGrant{Grant: authority.Grant{Domain: "simulation", PublicKey: pub}}})
	if err != nil {
		t.Fatal(err)
	}
	return NodeGateway{Client: c}, fixture
}
func inspectionReference(data []byte) sensing.EvidenceRef {
	sum := sha256.Sum256(data)
	return sensing.EvidenceRef{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data)), ContentType: "image/png"}
}
func putInspectionFixture(t *testing.T, store *evidence.Store, owner string, data []byte, until time.Time, class string) evidence.PutResponse {
	t.Helper()
	ref := inspectionReference(data)
	out, err := store.Put(owner, evidence.PutRequest{SHA256: ref.SHA256, Size: ref.Size, ContentType: ref.ContentType, Data: data, RetainUntilUnixMS: until.UnixMilli(), RetentionClass: class})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestInspectionEvidenceUsesRollingCaptureDeadlineAndRemainsEvictable(t *testing.T) {
	gateway, fixture := inspectionEvidenceFixture(t)
	data := bytes.Repeat([]byte("x"), evidence.MaxChunk+17)
	ref := inspectionReference(data)
	until := time.Now().Add(-200 * time.Millisecond).Add(7 * 24 * time.Hour)
	for range 2 {
		if err := gateway.StoreEvidence(context.Background(), ref, data, until); err != nil {
			t.Fatal(err)
		}
	}
	fixture.mu.Lock()
	puts := append([]evidence.PutRequest(nil), fixture.puts...)
	gets := fixture.gets
	fixture.mu.Unlock()
	if len(puts) != 4 || gets != 4 {
		t.Fatalf("expected initial probe/two chunks, retry probe and verified reads: %d %d", len(puts), gets)
	}
	for _, request := range puts {
		if request.RetentionClass != "rolling" || request.RetainUntilUnixMS != until.UnixMilli() {
			t.Fatal("inspection changed rolling policy or capture expiry", request.RetentionClass, request.RetainUntilUnixMS)
		}
	}
	putInspectionFixture(t, fixture.store, "camera", []byte("later capture"), until, "rolling")
	if _, err := fixture.store.Get(evidence.GetRequest{SHA256: ref.SHA256, Length: 1}); !errors.Is(err, evidence.ErrUnavailable) {
		t.Fatal("uncommitted candidate became protected", err)
	}
}
func TestInspectionEvidenceReusesProtectedCaptureWithoutChangingOwnerOrExpiry(t *testing.T) {
	gateway, fixture := inspectionEvidenceFixture(t)
	data := []byte("legacy captured image")
	ref := inspectionReference(data)
	originalUntil := time.Now().Add(time.Hour)
	original := putInspectionFixture(t, fixture.store, "camera", data, originalUntil, "")
	if err := gateway.StoreEvidence(context.Background(), ref, data, time.Now().Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	probe, err := fixture.store.Put("camera", evidence.PutRequest{SHA256: ref.SHA256, Size: ref.Size, ContentType: ref.ContentType, RetainUntilUnixMS: originalUntil.UnixMilli()})
	if err != nil || probe != original {
		t.Fatal("reuse changed original metadata", probe, err)
	}
	other := []byte("later image")
	otherRef := inspectionReference(other)
	if _, err = fixture.store.Put("camera", evidence.PutRequest{SHA256: otherRef.SHA256, Size: otherRef.Size, ContentType: otherRef.ContentType, Data: other, RetentionClass: "rolling"}); !errors.Is(err, evidence.ErrQuota) {
		t.Fatal("protected capture was downgraded", err)
	}
}
func TestInspectionEvidenceUploadAuthorityDenialCannotUseReadFallback(t *testing.T) {
	for _, code := range []int{401, 403} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			gateway, fixture := inspectionEvidenceFixture(t)
			data := []byte("existing image")
			putInspectionFixture(t, fixture.store, "camera", data, time.Now().Add(time.Hour), "")
			fixture.mu.Lock()
			fixture.denyUpload = code
			fixture.mu.Unlock()
			err := gateway.StoreEvidence(context.Background(), inspectionReference(data), data, time.Now().Add(7*24*time.Hour))
			var status *client.HTTPStatusError
			if !errors.As(err, &status) || status.StatusCode != code {
				t.Fatal("write denial hidden", err)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.gets != 0 || len(fixture.puts) != 1 {
				t.Fatal("authority denial reached optional read")
			}
		})
	}
}
