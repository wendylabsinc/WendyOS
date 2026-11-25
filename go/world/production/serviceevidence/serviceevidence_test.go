package serviceevidence

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

const tenant = "11111111-1111-4111-8111-111111111111"
const device = "spiffe://wendy.sh/tenant/" + tenant + "/device/22222222-2222-4222-8222-222222222222"

type testClock struct {
	mu            sync.Mutex
	w             trustedtime.Interval
	calls, failAt int
	invalid       bool
}

func newClock() *testClock {
	return &testClock{w: trustedtime.Interval{Earliest: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), Latest: time.Date(2026, 11, 11, 12, 0, 1, 0, time.UTC), BootID: "boot-a", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "unit-test-only"}}
}
func (c *testClock) Now() (trustedtime.Interval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls == c.failAt {
		return trustedtime.Interval{}, errors.New("clock unavailable")
	}
	w := c.w
	if c.invalid {
		w.Authenticated = false
	}
	return w, nil
}
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.w.Earliest = c.w.Earliest.Add(d)
	c.w.Latest = c.w.Latest.Add(d)
	c.w.Monotonic += d
}
func (c *testClock) failAfter(n int) { c.mu.Lock(); defer c.mu.Unlock(); c.failAt = c.calls + n }
func owner() Owner {
	return Owner{Tenant: tenant, Site: "office", Device: device, App: "com.example.sensors", Service: "camera-a", Component: "wendy.sensor.capture.v1", Stream: "front"}
}
func fixture(t *testing.T) (Config, *testClock) {
	t.Helper()
	base, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	clock := newClock()
	return Config{Directory: filepath.Join(base, "evidence"), Tenant: tenant, Site: "office", Device: device, Clock: clock, QuotaBytes: 1 << 20, MaxBlobBytes: 128 << 10, MaxEntries: 16, MaxRetention: time.Hour}, clock
}
func open(t *testing.T, c Config) *Store {
	t.Helper()
	s, e := Open(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func request(body []byte) PutRequest {
	return PutRequest{SHA256: digest(body), Size: int64(len(body)), Data: body, ContentType: "image/png"}
}

func TestOwnerBindingAcrossServicesAndRestart(t *testing.T) {
	cfg, clock := fixture(t)
	s := open(t, cfg)
	o := owner()
	body := []byte("verified rendered PNG fixture bytes")
	req := request(body)
	put, err := s.Put(context.Background(), o, req)
	if err != nil || !put.Complete {
		t.Fatalf("put: %v", err)
	}
	sha, _ := o.SHA256()
	if put.Owner != o || put.OwnerSHA256 != sha || put.RetainUntilUnixMS != clock.w.Latest.Add(time.Hour).UnixMilli() {
		t.Fatal("source/retention metadata not exact")
	}
	for _, change := range []func(*Owner){func(x *Owner) { x.Tenant = "33333333-3333-4333-8333-333333333333" }, func(x *Owner) { x.Site = "other" }, func(x *Owner) {
		x.Device = "spiffe://wendy.sh/tenant/" + tenant + "/device/33333333-3333-4333-8333-333333333333"
	}, func(x *Owner) { x.App = "com.other" }, func(x *Owner) { x.Service = "camera-b" }, func(x *Owner) { x.Component = "wendy.other.v1" }, func(x *Owner) { x.Stream = "rear" }} {
		wrong := o
		change(&wrong)
		got, e := s.Get(context.Background(), wrong, GetRequest{SHA256: req.SHA256, Length: MaxChunk})
		if e == nil || len(got.Data) != 0 || got.Owner != (Owner{}) {
			t.Fatal("digest disclosed bytes or owner metadata across scope")
		}
		ref, e := s.MatchComplete(context.Background(), wrong, req.SHA256, req.Size, req.ContentType)
		if e == nil || ref != (Reference{}) {
			t.Fatal("wrong owner matched")
		}
		p, e := s.Put(context.Background(), wrong, req)
		if e == nil || p != (PutResponse{}) {
			t.Fatal("another owner claimed same digest")
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	clock.advance(10 * time.Second)
	s = open(t, cfg)
	got, err := s.Get(context.Background(), o, GetRequest{SHA256: req.SHA256, Offset: 3, Length: 5})
	if err != nil || !bytes.Equal(got.Data, body[3:8]) || got.OwnerSHA256 != sha || got.RetainUntilUnixMS != put.RetainUntilUnixMS {
		t.Fatalf("restart read: %v", err)
	}
	if ref, err := s.MatchComplete(context.Background(), o, req.SHA256, req.Size, req.ContentType); err != nil || ref != put.Reference {
		t.Fatalf("restart completion: %v", err)
	}
	// Observation TTL is deliberately absent from evidence ownership/retention.
	clock.advance(5 * time.Minute)
	if _, err = s.Get(context.Background(), o, GetRequest{SHA256: req.SHA256, Length: MaxChunk}); err != nil {
		t.Fatal("retained media disappeared independently of observation lifetime", err)
	}
}

func TestCompleteReadExactBytesBoundsOwnerAndIntegrity(t *testing.T) {
	cfg, clock := fixture(t)
	s := open(t, cfg)
	body := bytes.Repeat([]byte("whole-blob-schema-validation"), 2100)
	r := request(body)
	r.Data = body[:MaxChunk]
	if _, err := s.Put(context.Background(), owner(), r); err != nil {
		t.Fatal(err)
	}
	if ref, data, err := s.ReadComplete(context.Background(), owner(), r.SHA256, r.Size, r.ContentType); err == nil || ref != (Reference{}) || len(data) != 0 {
		t.Fatal("incomplete whole blob exposed")
	}
	r.Offset, r.Data = MaxChunk, body[MaxChunk:]
	put, err := s.Put(context.Background(), owner(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	s = open(t, cfg)
	ref, data, err := s.ReadComplete(context.Background(), owner(), r.SHA256, r.Size, r.ContentType)
	if err != nil || ref != put.Reference || !bytes.Equal(data, body) {
		t.Fatal("whole blob read changed bytes/metadata after restart", err)
	}
	data[0] ^= 1
	_, again, err := s.ReadComplete(context.Background(), owner(), r.SHA256, r.Size, r.ContentType)
	if err != nil || !bytes.Equal(again, body) {
		t.Fatal("caller mutated retained bytes", err)
	}
	wrong := owner()
	wrong.Service = "camera-b"
	for _, q := range []struct {
		o    Owner
		size int64
		mime string
	}{{wrong, r.Size, r.ContentType}, {owner(), 0, r.ContentType}, {owner(), cfg.MaxBlobBytes + 1, r.ContentType}, {owner(), r.Size - 1, r.ContentType}, {owner(), r.Size, "image/jpeg"}} {
		if ref, data, err := s.ReadComplete(context.Background(), q.o, r.SHA256, q.size, q.mime); err == nil || ref != (Reference{}) || len(data) != 0 {
			t.Fatal("invalid whole blob read exposed bytes/owner")
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.Directory, "blobs", r.SHA256+".blob"), bytes.Repeat([]byte("x"), len(body)), 0600); err != nil {
		t.Fatal(err)
	}
	if ref, data, err := s.ReadComplete(context.Background(), owner(), r.SHA256, r.Size, r.ContentType); err == nil || ref != (Reference{}) || len(data) != 0 {
		t.Fatal("corrupt whole blob exposed")
	}
}

func TestCompleteReadPostClockFailureAndExpiryExposeNothing(t *testing.T) {
	for _, mode := range []string{"post-clock", "expired"} {
		t.Run(mode, func(t *testing.T) {
			cfg, clock := fixture(t)
			s := open(t, cfg)
			r := request([]byte("whole-blob"))
			if _, err := s.Put(context.Background(), owner(), r); err != nil {
				t.Fatal(err)
			}
			if mode == "post-clock" {
				clock.failAfter(3)
			} else {
				clock.advance(2 * time.Hour)
			}
			if ref, data, err := s.ReadComplete(context.Background(), owner(), r.SHA256, r.Size, r.ContentType); err == nil || ref != (Reference{}) || len(data) != 0 {
				t.Fatal("whole blob outlived authority/retention")
			}
		})
	}
}

func TestIncompleteChunksRetryConflictAndImmutableDeadline(t *testing.T) {
	cfg, clock := fixture(t)
	s := open(t, cfg)
	o := owner()
	body := bytes.Repeat([]byte("changing-camera-frame"), 4000)
	req := request(body)
	req.Data = body[:MaxChunk]
	first, err := s.Put(context.Background(), o, req)
	if err != nil || first.Complete || first.Received != MaxChunk {
		t.Fatalf("partial: %v", err)
	}
	if _, err = s.Get(context.Background(), o, GetRequest{SHA256: req.SHA256, Length: 10}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("incomplete data readable")
	}
	if _, err = s.MatchComplete(context.Background(), o, req.SHA256, req.Size, req.ContentType); !errors.Is(err, ErrUnavailable) {
		t.Fatal("incomplete evidence matched")
	}
	clock.advance(time.Second)
	retry, err := s.Put(context.Background(), o, req)
	if err != nil || retry.Received != first.Received || retry.RetainUntilUnixMS != first.RetainUntilUnixMS {
		t.Fatal("retry changed committed offset/deadline", err)
	}
	bad := req
	bad.Data = bytes.Repeat([]byte("x"), MaxChunk)
	if _, err = s.Put(context.Background(), o, bad); err == nil {
		t.Fatal("conflicting retried bytes accepted")
	}
	bad = req
	bad.RetainUntilUnixMS = first.RetainUntilUnixMS + 1000
	if _, err = s.Put(context.Background(), o, bad); err == nil {
		t.Fatal("retention renewed")
	}
	bad = req
	bad.Offset = first.Received + 1
	bad.Data = nil
	if _, err = s.Put(context.Background(), o, bad); err == nil {
		t.Fatal("upload gap accepted")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, cfg)
	req.Offset = MaxChunk
	req.Data = body[MaxChunk:]
	done, err := s.Put(context.Background(), o, req)
	if err != nil || !done.Complete || done.RetainUntilUnixMS != first.RetainUntilUnixMS {
		t.Fatalf("resumed upload: %v", err)
	}
	if _, err = s.MatchComplete(context.Background(), o, req.SHA256, req.Size+1, req.ContentType); err == nil {
		t.Fatal("wrong size matched")
	}
	if _, err = s.MatchComplete(context.Background(), o, req.SHA256, req.Size, "audio/wav"); err == nil {
		t.Fatal("wrong MIME matched")
	}
	clock.advance(2 * time.Hour)
	if _, err = s.Get(context.Background(), o, GetRequest{SHA256: req.SHA256, Length: 10}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("expired bytes served", err)
	}
}

func TestDigestFailureAndCorruptDiskFailClosed(t *testing.T) {
	for _, kind := range []string{"wrong-upload", "corrupt-body", "missing-metadata", "corrupt-metadata", "missing-blob-directory"} {
		t.Run(kind, func(t *testing.T) {
			cfg, _ := fixture(t)
			s := open(t, cfg)
			o := owner()
			body := []byte("rendered bytes")
			req := request(body)
			if kind == "wrong-upload" {
				req.Data = []byte("different data")
				req.Size = int64(len(req.Data))
				if _, err := s.Put(context.Background(), o, req); err == nil {
					t.Fatal("wrong digest committed")
				}
				return
			}
			if _, err := s.Put(context.Background(), o, req); err != nil {
				t.Fatal(err)
			}
			if kind == "corrupt-body" {
				if err := os.WriteFile(filepath.Join(cfg.Directory, "blobs", req.SHA256+".blob"), bytes.Repeat([]byte("x"), len(body)), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Get(context.Background(), o, GetRequest{SHA256: req.SHA256, Length: 10}); err == nil {
					t.Fatal("corrupt bytes returned")
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing-metadata":
				if err := os.Remove(filepath.Join(cfg.Directory, stateName)); err != nil {
					t.Fatal(err)
				}
			case "corrupt-metadata":
				if err := os.WriteFile(filepath.Join(cfg.Directory, stateName), []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-blob-directory":
				if err := os.Rename(filepath.Join(cfg.Directory, "blobs"), filepath.Join(filepath.Dir(cfg.Directory), "detached-blobs")); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := Open(cfg); err == nil {
				got.Close()
				t.Fatal("corrupt history opened or recreated")
			}
		})
	}
}

func TestClockFaultBeforeAfterAndDurableRegression(t *testing.T) {
	t.Run("before", func(t *testing.T) {
		cfg, c := fixture(t)
		s := open(t, cfg)
		c.failAfter(1)
		if _, e := s.Put(context.Background(), owner(), request([]byte("bytes"))); !errors.Is(e, ErrClock) {
			t.Fatal(e)
		}
		if _, e := s.Put(context.Background(), owner(), request([]byte("bytes"))); !errors.Is(e, ErrFaulted) {
			t.Fatal("fault silently recovered")
		}
	})
	t.Run("after", func(t *testing.T) {
		cfg, c := fixture(t)
		s := open(t, cfg)
		r := request([]byte("durable uncertain bytes"))
		c.failAfter(3)
		p, e := s.Put(context.Background(), owner(), r)
		if !errors.Is(e, ErrUncertain) || !errors.Is(e, ErrClock) || p != (PutResponse{}) {
			t.Fatalf("post-commit clock failure acknowledged: %v", e)
		}
		s.Close()
		c.advance(time.Second)
		s = open(t, cfg)
		got, e := s.Get(context.Background(), owner(), GetRequest{SHA256: r.SHA256, Length: MaxChunk})
		if e != nil || !bytes.Equal(got.Data, r.Data) {
			t.Fatal("unknown outcome lost durable bytes", e)
		}
	})
	t.Run("read-after", func(t *testing.T) {
		cfg, c := fixture(t)
		s := open(t, cfg)
		r := request([]byte("private bytes"))
		if _, e := s.Put(context.Background(), owner(), r); e != nil {
			t.Fatal(e)
		}
		c.failAfter(3)
		got, e := s.Get(context.Background(), owner(), GetRequest{SHA256: r.SHA256, Length: MaxChunk})
		if !errors.Is(e, ErrClock) || len(got.Data) != 0 {
			t.Fatal("read exposed after clock failure", e)
		}
	})
	t.Run("new-boot-rollback", func(t *testing.T) {
		cfg, c := fixture(t)
		s := open(t, cfg)
		c.advance(time.Minute)
		if _, e := s.Put(context.Background(), owner(), request([]byte("bytes"))); e != nil {
			t.Fatal(e)
		}
		s.Close()
		c.advance(-time.Minute)
		c.w.BootID = "boot-b"
		c.w.Monotonic = 0
		if got, e := Open(cfg); e == nil {
			got.Close()
			t.Fatal("new boot accepted time behind durable floor")
		}
	})
	t.Run("untrusted", func(t *testing.T) {
		cfg, c := fixture(t)
		c.invalid = true
		if got, e := Open(cfg); e == nil {
			got.Close()
			t.Fatal("unknown clock accepted")
		}
	})
}

func TestOwnerHashQuotaScopeAndUnsafePaths(t *testing.T) {
	o := owner()
	h, e := o.SHA256()
	if e != nil || len(h) != 64 {
		t.Fatal(e)
	}
	other := o
	other.Stream = "rear"
	h2, _ := other.SHA256()
	if h == h2 {
		t.Fatal("tuple collision")
	}
	other.Stream = "bad\x00stream"
	if _, e = other.SHA256(); e == nil {
		t.Fatal("delimiter injection accepted")
	}
	cfg, _ := fixture(t)
	cfg.MaxEntries = 1
	s := open(t, cfg)
	if _, e = s.Put(context.Background(), o, request([]byte("one"))); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Put(context.Background(), o, request([]byte("two"))); !errors.Is(e, ErrQuota) {
		t.Fatalf("quota: %v", e)
	}
	if got, e := Open(cfg); e == nil {
		got.Close()
		t.Fatal("concurrent opener accepted")
	}
	s.Close()
	wrong := cfg
	wrong.Site = "another-site"
	if got, e := Open(wrong); e == nil {
		got.Close()
		t.Fatal("scope changed on reopen")
	}
	unsafe, _ := fixture(t)
	if err := os.MkdirAll(unsafe.Directory, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe.Directory, 0777); err != nil {
		t.Fatal(err)
	}
	if got, e := Open(unsafe); e == nil {
		got.Close()
		t.Fatal("unsafe writable directory accepted")
	}
	symlink, _ := fixture(t)
	target := filepath.Join(filepath.Dir(symlink.Directory), "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, symlink.Directory); err != nil {
		t.Fatal(err)
	}
	if got, e := Open(symlink); e == nil {
		got.Close()
		t.Fatal("symlink directory accepted")
	}
}
