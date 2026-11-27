package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"
)

func TestOwnerScopedReadAndCompletionSurviveRestart(t *testing.T) {
	now := time.Now()
	cfg := Config{Tenant: "tenant", Site: "site", Now: func() time.Time { return now }}
	dir := t.TempDir()
	s, err := Open(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("owned capture bytes")
	h := sha256.Sum256(body)
	digest := hex.EncodeToString(h[:])
	put := PutRequest{SHA256: digest, Size: int64(len(body)), Data: body, ContentType: "application/octet-stream"}
	if _, err = s.Put("source-a", put); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	get := GetRequest{SHA256: digest, Length: MaxChunk}
	ref := Reference{SHA256: digest, Size: int64(len(body)), ContentType: put.ContentType}
	if _, err = s.GetOwned("source-b", get); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wrong owner: %v", err)
	}
	if _, err = s.CompleteOwned("source-b", ref); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wrong owner completion: %v", err)
	}
	got, err := s.GetOwned("source-a", get)
	if err != nil || !bytes.Equal(got.Data, body) {
		t.Fatalf("owned bytes: %v", err)
	}
	if _, err = s.Get(get); err != nil {
		t.Fatalf("existing site-wide API changed: %v", err)
	}
	if m, err := s.CompleteOwned("source-a", ref); err != nil || !m.Complete {
		t.Fatalf("complete: %v", err)
	}
	if m, data, err := s.ReadCompleteOwned("source-a", ref); err != nil || !m.Complete || !bytes.Equal(data, body) {
		t.Fatalf("complete bytes: %v", err)
	}
	if m, data, err := s.ReadCompleteOwned("source-b", ref); err == nil || m != (PutResponse{}) || len(data) != 0 {
		t.Fatal("whole blob exposed across owner")
	}
	ref.Size++
	if _, err = s.CompleteOwned("source-a", ref); !errors.Is(err, ErrUnavailable) {
		t.Fatal("mismatched size accepted")
	}
	ref.Size--
	if err = os.WriteFile(s.blob(digest), bytes.Repeat([]byte("x"), len(body)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteOwned("source-a", ref); err == nil {
		t.Fatal("corrupt complete bytes accepted")
	}
	if m, data, err := s.ReadCompleteOwned("source-a", ref); err == nil || m != (PutResponse{}) || len(data) != 0 {
		t.Fatal("corrupt whole blob exposed")
	}
}

func TestOwnerScopedCompletionRefusesIncompleteAndExpired(t *testing.T) {
	now := time.Now()
	s, err := Open(t.TempDir(), Config{Tenant: "tenant", Site: "site", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	body := []byte("two chunks")
	h := sha256.Sum256(body)
	digest := hex.EncodeToString(h[:])
	until := now.Add(time.Minute).UnixMilli()
	r := PutRequest{SHA256: digest, Size: int64(len(body)), Data: body[:3], ContentType: "application/octet-stream", RetainUntilUnixMS: until}
	if _, err = s.Put("source", r); err != nil {
		t.Fatal(err)
	}
	ref := Reference{SHA256: digest, Size: r.Size, ContentType: r.ContentType}
	if _, err = s.CompleteOwned("source", ref); !errors.Is(err, ErrUnavailable) {
		t.Fatal("incomplete accepted")
	}
	r.Offset = 3
	r.Data = body[3:]
	if _, err = s.Put("source", r); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err = s.GetOwned("source", GetRequest{SHA256: digest, Length: 10}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("expired accepted")
	}
}
