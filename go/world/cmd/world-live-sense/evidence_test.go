package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

type storeClient struct {
	store       *evidence.Store
	owner       string
	read        bool
	reads       int
	uploads     int
	uploadError error
	mutate      func(*evidence.GetResponse)
}

func (c *storeClient) UploadRollingEvidence(_ context.Context, data []byte, contentType string, until time.Time) (evidence.PutResponse, error) {
	if until.IsZero() {
		return evidence.PutResponse{}, errors.New("rolling retention must be fixed")
	}
	c.uploads++
	if c.uploadError != nil {
		return evidence.PutResponse{}, c.uploadError
	}
	h := sha256.Sum256(data)
	return c.store.Put(c.owner, evidence.PutRequest{SHA256: hex.EncodeToString(h[:]), Size: int64(len(data)), Data: data, ContentType: contentType, RetainUntilUnixMS: until.UnixMilli(), RetentionClass: "rolling"})
}
func (c *storeClient) DownloadEvidence(_ context.Context, hash string, max int64) ([]byte, evidence.GetResponse, error) {
	c.reads++
	if !c.read {
		return nil, evidence.GetResponse{}, errors.New("node HTTP 403")
	}
	m, err := c.store.Get(evidence.GetRequest{SHA256: hash, Length: int(max)})
	if c.mutate != nil {
		c.mutate(&m)
	}
	return m.Data, m, err
}
func evidenceFixture(t *testing.T) (*evidence.Store, sensing.EvidenceRef, []byte, time.Time) {
	t.Helper()
	s, err := evidence.Open(t.TempDir(), evidence.Config{Tenant: "test", Site: "office"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	data := []byte("immutable rendered frame")
	h := sha256.Sum256(data)
	return s, sensing.EvidenceRef{SHA256: hex.EncodeToString(h[:]), Size: int64(len(data)), ContentType: "image/png"}, data, time.Now().Add(time.Hour)
}
func TestEvidenceWriteOnlyPublisherDoesNotRead(t *testing.T) {
	s, ref, data, until := evidenceFixture(t)
	c := &storeClient{store: s, owner: "sensor"}
	first, err := storeEvidence(context.Background(), c, ref, data, until)
	if err != nil {
		t.Fatal(err)
	}
	again, err := storeEvidence(context.Background(), c, ref, data, until)
	if err != nil || again != first || c.reads != 0 || c.uploads != 2 {
		t.Fatalf("write-only retry changed expiry or read: %d %d %d %v", first, again, c.reads, err)
	}
}
func TestEvidenceReusePreservesOtherOwnerAndRetention(t *testing.T) {
	s, ref, data, until := evidenceFixture(t)
	executor := &storeClient{store: s, owner: "executor", read: true}
	first, err := storeEvidence(context.Background(), executor, ref, data, until)
	if err != nil {
		t.Fatal(err)
	}
	publisher := &storeClient{store: s, owner: "sensor", read: true}
	got, err := storeEvidence(context.Background(), publisher, ref, data, until)
	if err != nil || got != first || publisher.reads != 1 {
		t.Fatalf("failed owner reuse: %d %v", got, err)
	}
	if _, err := publisher.UploadRollingEvidence(context.Background(), data, ref.ContentType, until); err == nil {
		t.Fatal("reuse transferred write ownership")
	}
	if _, err := executor.UploadRollingEvidence(context.Background(), data, ref.ContentType, until); err != nil {
		t.Fatal("reuse changed original owner", err)
	}
	writeOnly := &storeClient{store: s, owner: "other-sensor"}
	if _, err := storeEvidence(context.Background(), writeOnly, ref, data, until); err == nil {
		t.Fatal("reused without read permission")
	}
}
func TestEvidenceConcurrentOwnersCanBothUseVerifiedBlob(t *testing.T) {
	s, ref, data, until := evidenceFixture(t)
	a := &storeClient{store: s, owner: "sensor", read: true}
	b := &storeClient{store: s, owner: "executor", read: true}
	start := make(chan struct{})
	results := make(chan int64, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, c := range []*storeClient{a, b} {
		wg.Add(1)
		go func(c *storeClient) {
			defer wg.Done()
			<-start
			until, err := storeEvidence(context.Background(), c, ref, data, until)
			results <- until
			errs <- err
		}(c)
	}
	close(start)
	wg.Wait()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if x, y := <-results, <-results; x != y || x <= time.Now().UnixMilli() {
		t.Fatal("race changed retention")
	}
	if a.reads+b.reads != 1 {
		t.Fatal("expected exactly one losing writer to verify existing bytes")
	}
}
func TestEvidenceReuseRejectsInvalidBytesMetadataAndExpiry(t *testing.T) {
	for name, mutate := range map[string]func(*evidence.GetResponse){
		"bytes":  func(m *evidence.GetResponse) { m.Data[0] ^= 1 },
		"hash":   func(m *evidence.GetResponse) { m.SHA256 = "wrong" },
		"size":   func(m *evidence.GetResponse) { m.Size++ },
		"type":   func(m *evidence.GetResponse) { m.ContentType = "audio/wav" },
		"expiry": func(m *evidence.GetResponse) { m.RetainUntilUnixMS = time.Now().Add(-time.Second).UnixMilli() },
	} {
		t.Run(name, func(t *testing.T) {
			s, ref, data, until := evidenceFixture(t)
			owner := &storeClient{store: s, owner: "executor"}
			if _, err := storeEvidence(context.Background(), owner, ref, data, until); err != nil {
				t.Fatal(err)
			}
			c := &storeClient{store: s, owner: "sensor", read: true, mutate: mutate}
			if _, err := storeEvidence(context.Background(), c, ref, data, until); err == nil {
				t.Fatal("accepted invalid existing evidence")
			}
		})
	}
}

func TestRollingEvidenceReprobesAfterQuotaEvictionWithoutRenewingExpiry(t *testing.T) {
	s, err := evidence.Open(t.TempDir(), evidence.Config{Tenant: "test", Site: "office", MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	publisher := &storeClient{store: s, owner: "camera"}
	until := time.Now().Add(time.Hour)
	data := []byte("first capture")
	hash := sha256.Sum256(data)
	ref := sensing.EvidenceRef{SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data)), ContentType: "image/png"}
	first, err := storeEvidence(context.Background(), publisher, ref, data, until)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publisher.UploadRollingEvidence(context.Background(), []byte("second capture"), "image/png", until); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(evidence.GetRequest{SHA256: ref.SHA256, Length: len(data)}); !errors.Is(err, evidence.ErrUnavailable) {
		t.Fatalf("quota did not evict first capture: %v", err)
	}
	again, err := storeEvidence(context.Background(), publisher, ref, data, until)
	if err != nil || again != first || again != until.UnixMilli() || publisher.reads != 0 {
		t.Fatalf("reupload changed immutable retention: %d %d %v", first, again, err)
	}
	stored, err := s.Get(evidence.GetRequest{SHA256: ref.SHA256, Length: len(data)})
	if err != nil || string(stored.Data) != string(data) {
		t.Fatal("reprobe failed to restore evidence", err)
	}
}

func TestDuplicateCaptureCannotExtendExistingEvidenceExpiry(t *testing.T) {
	s, ref, data, until := evidenceFixture(t)
	publisher := &storeClient{store: s, owner: "camera", read: true}
	first, err := storeEvidence(context.Background(), publisher, ref, data, until)
	if err != nil {
		t.Fatal(err)
	}
	// After restart the bounded metadata cache is absent. A new capture of
	// identical bytes must verify the stored object and adopt its old expiry.
	again, err := storeEvidence(context.Background(), publisher, ref, data, until.Add(time.Minute))
	if err != nil || again != first || publisher.reads != 1 {
		t.Fatalf("new capture renewed existing evidence: %d %d %v", first, again, err)
	}
}

func TestEvidenceReadFallbackCannotHideWriteAuthorityDenial(t *testing.T) {
	s, ref, data, until := evidenceFixture(t)
	writer := &storeClient{store: s, owner: "sensor", read: true}
	if _, err := storeEvidence(context.Background(), writer, ref, data, until); err != nil {
		t.Fatal(err)
	}
	for _, code := range []int{401, 403} {
		denied := &client.HTTPStatusError{StatusCode: code}
		writer.uploadError = denied
		if _, err := storeEvidence(context.Background(), writer, ref, data, until); !errors.Is(err, denied) || writer.reads != 0 {
			t.Fatal("read hid write authority denial", err)
		}
	}
}
