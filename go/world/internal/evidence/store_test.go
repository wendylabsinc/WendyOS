package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func request(b []byte) PutRequest {
	h := sha256.Sum256(b)
	return PutRequest{SHA256: hex.EncodeToString(h[:]), Size: int64(len(b)), Data: b, ContentType: "image/png"}
}
func config(now *time.Time) Config {
	return Config{Tenant: "tenant", Site: "site", QuotaBytes: 4 << 20, MaxBlobBytes: 2 << 20, Now: func() time.Time { return *now }}
}
func TestResumeIdempotenceOwnerAndDigest(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	cfg := config(&now)
	s, e := Open(dir, cfg)
	if e != nil {
		t.Fatal(e)
	}
	bytesAll := bytes.Repeat([]byte("12345"), 20000)
	r := request(bytesAll)
	r.Data = bytesAll[:MaxChunk]
	first, e := s.Put("camera", r)
	if e != nil || first.Complete || first.Received != MaxChunk {
		t.Fatal(first, e)
	}
	if _, e = s.Get(GetRequest{SHA256: r.SHA256, Length: 1}); !errors.Is(e, ErrUnavailable) {
		t.Fatal("partial visible", e)
	}
	if again, e := s.Put("camera", r); e != nil || again != first {
		t.Fatal("retry", again, e)
	}
	if _, e = s.Put("different", r); e == nil {
		t.Fatal("owner hijack")
	}
	bad := r
	bad.Offset = MaxChunk + 1
	if _, e = s.Put("camera", bad); e == nil {
		t.Fatal("gap")
	}
	// Bytes beyond the committed offset model crash between body fsync and manifest.
	f, e := os.OpenFile(s.blob(r.SHA256), os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = f.Write([]byte("unacknowledged"))
	f.Close()
	s.Close()
	s, e = Open(dir, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	for offset := int64(MaxChunk); offset < int64(len(bytesAll)); {
		end := offset + MaxChunk
		if end > int64(len(bytesAll)) {
			end = int64(len(bytesAll))
		}
		r.Offset = offset
		r.Data = bytesAll[offset:end]
		r.RetainUntilUnixMS = first.RetainUntilUnixMS
		out, e := s.Put("camera", r)
		if e != nil {
			t.Fatal(e)
		}
		offset = out.Received
	}
	var got []byte
	for offset := int64(0); offset < r.Size; {
		part, e := s.Get(GetRequest{SHA256: r.SHA256, Offset: offset, Length: MaxChunk})
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, part.Data...)
		offset += int64(len(part.Data))
	}
	if !bytes.Equal(got, bytesAll) {
		t.Fatal("bytes changed")
	}
	if e = os.WriteFile(s.blob(r.SHA256), bytes.Repeat([]byte("x"), len(bytesAll)), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Get(GetRequest{SHA256: r.SHA256, Length: 10}); e == nil {
		t.Fatal("corrupt bytes served")
	}
	s.Close()
	if broken, e := Open(dir, cfg); e == nil {
		broken.Close()
		t.Fatal("reopened corrupt blob")
	}
}
func TestExpiryQuotaAndInvalidChunks(t *testing.T) {
	now := time.Now()
	cfg := config(&now)
	cfg.QuotaBytes = 10
	cfg.MaxBlobBytes = 10
	cfg.MaxRetention = time.Hour
	s, e := Open(t.TempDir(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r := request([]byte("123456"))
	r.RetainUntilUnixMS = now.Add(time.Second).UnixMilli()
	if _, e = s.Put("a", r); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Put("a", request([]byte("abcdef"))); !errors.Is(e, ErrQuota) {
		t.Fatal(e)
	}
	now = now.Add(time.Second)
	if _, e = s.Get(GetRequest{SHA256: r.SHA256, Length: 1}); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	if _, e = s.Put("a", request([]byte("abcdef"))); e != nil {
		t.Fatal(e)
	}
	bad := request([]byte("z"))
	bad.SHA256 = string(bytes.Repeat([]byte("0"), 64))
	if _, e = s.Put("a", bad); e == nil {
		t.Fatal("wrong digest committed")
	}
	for _, bad := range []PutRequest{{SHA256: "../../escape"}, {SHA256: r.SHA256, Size: 1, Data: []byte("x"), ContentType: "text/html"}, {SHA256: r.SHA256, Size: 11, ContentType: "image/png"}} {
		if _, e = s.Put("a", bad); e == nil {
			t.Fatal("bad request accepted")
		}
	}
}
func TestScopeLockAndSymlink(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	cfg := config(&now)
	s, e := Open(dir, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if other, e := Open(dir, cfg); e == nil {
		other.Close()
		t.Fatal("double lock")
	}
	r := request([]byte("data"))
	if _, e = s.Put("a", r); e != nil {
		t.Fatal(e)
	}
	s.Close()
	cfg.Site = "other"
	if wrong, e := Open(dir, cfg); e == nil {
		wrong.Close()
		t.Fatal("scope changed")
	}
	cfg.Site = "site"
	if e = os.Remove(filepath.Join(dir, r.SHA256+".blob")); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink("/etc/passwd", filepath.Join(dir, r.SHA256+".blob")); e != nil {
		t.Fatal(e)
	}
	if bad, e := Open(dir, cfg); e == nil {
		bad.Close()
		t.Fatal("symlink opened")
	}
}

func TestCompletedProbeRechecksCurrentBytes(t *testing.T) {
	for _, remove := range []bool{false, true} {
		name := "corruption"
		if remove {
			name = "missing"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			s, err := Open(t.TempDir(), config(&now))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r := request([]byte("capture"))
			if out, e := s.Put("camera", r); e != nil || !out.Complete {
				t.Fatal(out, e)
			}
			if remove {
				err = os.Remove(s.blob(r.SHA256))
			} else {
				err = os.WriteFile(s.blob(r.SHA256), []byte("changed"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			// Empty probe is how the resumable client detects already complete uploads.
			probe := r
			probe.Data = nil
			if out, e := s.Put("camera", probe); e == nil || out.Complete {
				t.Fatal("corrupt completion acknowledged", out, e)
			}
			if out, e := s.Put("camera", r); e == nil || out.Complete {
				t.Fatal("corrupt retry acknowledged", out, e)
			}
		})
	}
}
