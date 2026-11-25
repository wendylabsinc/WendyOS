package packagecache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
)

func fixture() (Config, []byte) {
	return Config{Scope: Scope{Tenant: "tenant", Site: "office", Domain: "simulation", Audience: "node", Endpoint: "http://localhost"}}, bytes.Repeat([]byte("private synthetic package bytes\n"), 4000)
}

func getter(data []byte, offsets *[]int64) Getter {
	until := time.Now().Add(time.Hour).UnixMilli()
	return func(_ context.Context, r evidence.GetRequest) (evidence.GetResponse, error) {
		if offsets != nil {
			*offsets = append(*offsets, r.Offset)
		}
		if r.Offset < 0 || r.Offset >= int64(len(data)) {
			return evidence.GetResponse{}, errors.New("bad range")
		}
		end := min(int64(len(data)), r.Offset+int64(r.Length))
		return evidence.GetResponse{SHA256: digest(data), Size: int64(len(data)), Offset: r.Offset, Data: append([]byte(nil), data[r.Offset:end]...), ContentType: "application/octet-stream", RetainUntilUnixMS: until}, nil
	}
}

func open(t *testing.T, path string, cfg Config) *Cache {
	t.Helper()
	c, err := Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestResumeCommittedPrefixAcrossReopen(t *testing.T) {
	cfg, data := fixture()
	path := filepath.Join(t.TempDir(), "cache")
	c := open(t, path, cfg)
	hash := digest(data)
	var offsets []int64
	get := getter(data, &offsets)
	fail := true
	flaky := func(ctx context.Context, r evidence.GetRequest) (evidence.GetResponse, error) {
		if fail && r.Offset >= evidence.MaxChunk {
			return evidence.GetResponse{}, errors.New("link lost")
		}
		return get(ctx, r)
	}
	if _, _, err := c.Fetch(context.Background(), hash, int64(len(data)), flaky); err == nil {
		t.Fatal("interruption not surfaced")
	}
	state, _ := c.Inventory()
	if len(state.Entries) != 1 || state.Entries[0].Received != evidence.MaxChunk || state.Entries[0].Complete {
		t.Fatal(state)
	}
	c.Close()
	c = open(t, path, cfg)
	offsets = nil
	fail = false
	got, progress, err := c.Fetch(context.Background(), hash, int64(len(data)), flaky)
	if err != nil || !bytes.Equal(got, data) || offsets[0] != evidence.MaxChunk || progress.ResumedBytes != evidence.MaxChunk || progress.DownloadedBytes != int64(len(data))-evidence.MaxChunk {
		t.Fatal(progress, offsets, err)
	}
	offsets = nil
	got, progress, err = c.Fetch(context.Background(), hash, int64(len(data)), get)
	if err != nil || !bytes.Equal(got, data) || !progress.CacheHit || progress.DownloadedBytes != 1 || !reflect.DeepEqual(offsets, []int64{0}) {
		t.Fatal(progress, offsets, err)
	}
}

func TestCompleteCacheStillRequiresCurrentAccessAndMetadata(t *testing.T) {
	for _, variant := range []string{"denied", "expired", "changed-size", "changed-retention", "changed-type", "wrong-first-byte", "cancelled"} {
		t.Run(variant, func(t *testing.T) {
			cfg, data := fixture()
			c := open(t, filepath.Join(t.TempDir(), "cache"), cfg)
			get := getter(data, nil)
			hash := digest(data)
			if _, _, err := c.Fetch(context.Background(), hash, int64(len(data)), get); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bad := func(ctx context.Context, r evidence.GetRequest) (evidence.GetResponse, error) {
				v, err := get(ctx, r)
				switch variant {
				case "denied":
					return v, errors.New("revoked read grant")
				case "expired":
					v.RetainUntilUnixMS = 1
				case "changed-size":
					v.Size++
				case "changed-retention":
					v.RetainUntilUnixMS++
				case "changed-type":
					v.ContentType = "text/plain"
				case "wrong-first-byte":
					v.Data[0] ^= 1
				case "cancelled":
					cancel()
				}
				return v, err
			}
			if got, _, err := c.Fetch(ctx, hash, int64(len(data))+1, bad); err == nil || got != nil {
				t.Fatal("cached bytes escaped access/metadata check")
			}
		})
	}
}

func TestCacheScopeLockAndQuotas(t *testing.T) {
	cfg, data := fixture()
	cfg.MaxBytes = int64(len(data))
	cfg.MaxEntries = 1
	path := filepath.Join(t.TempDir(), "cache")
	c := open(t, path, cfg)
	if other, err := Open(path, cfg); !errors.Is(err, ErrLocked) {
		if other != nil {
			other.Close()
		}
		t.Fatal(err)
	}
	if _, _, err := c.Fetch(context.Background(), digest(data), int64(len(data)), getter(data, nil)); err != nil {
		t.Fatal(err)
	}
	other := []byte("another blob")
	if _, _, err := c.Fetch(context.Background(), digest(other), int64(len(other)), getter(other, nil)); !errors.Is(err, ErrQuota) {
		t.Fatal(err)
	}
	if err := c.Discard(digest(data)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Fetch(context.Background(), digest(other), int64(len(other)), getter(other, nil)); err != nil {
		t.Fatal(err)
	}
	c.Close()
	changed := cfg
	changed.Scope.Site = "another-site"
	if _, err := Open(path, changed); err == nil {
		t.Fatal("cross-site cache adopted")
	}
	changed = cfg
	changed.Scope.Endpoint = "http://other-node"
	if _, err := Open(path, changed); err == nil {
		t.Fatal("cross-endpoint cache adopted")
	}
	state, err := Inspect(path)
	if err != nil || len(state.Entries) != 1 || state.StoredBytes != int64(len(other)) {
		t.Fatal(state, err)
	}
	if err = Discard(path, digest(other)); err != nil {
		t.Fatal(err)
	}
	state, err = Inspect(path)
	if err != nil || len(state.Entries) != 0 {
		t.Fatal(state, err)
	}
}

func TestCacheRejectsCorruptBytesSymlinksAndPublicDirectories(t *testing.T) {
	for _, kind := range []string{"tamper", "truncated", "data-link", "root-link", "public", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			cfg, data := fixture()
			base := t.TempDir()
			path := filepath.Join(base, "cache")
			c := open(t, path, cfg)
			if _, _, err := c.Fetch(context.Background(), digest(data), int64(len(data)), getter(data, nil)); err != nil {
				t.Fatal(err)
			}
			c.Close()
			file := filepath.Join(path, digest(data)+".data")
			switch kind {
			case "tamper":
				data[0] ^= 1
				os.WriteFile(file, data, 0600)
			case "truncated":
				os.Truncate(file, 1)
			case "data-link":
				os.Remove(file)
				os.Symlink(filepath.Join(base, "outside"), file)
			case "root-link":
				alias := filepath.Join(base, "alias")
				os.Symlink(path, alias)
				path = alias
			case "public":
				os.Chmod(path, 0755)
			case "hardlink":
				os.Link(file, filepath.Join(base, "alias"))
			}
			if reopened, err := Open(path, cfg); err == nil {
				reopened.Close()
				t.Fatal("unsafe cache reopened")
			}
		})
	}
}

func TestUncertainCheckpointPoisonsUntilReopen(t *testing.T) {
	cfg, data := fixture()
	path := filepath.Join(t.TempDir(), "cache")
	c := open(t, path, cfg)
	get := getter(data, nil)
	c.afterWrite = func(stage string) error {
		if stage == "after-checkpoint-rename" {
			return errors.New("simulated directory sync failure")
		}
		return nil
	}
	if _, _, err := c.Fetch(context.Background(), digest(data), int64(len(data)), get); err == nil {
		t.Fatal("uncertain write accepted")
	}
	if _, _, err := c.Fetch(context.Background(), digest(data), int64(len(data)), get); !errors.Is(err, ErrFaulted) {
		t.Fatal(err)
	}
	c.Close()
	c = open(t, path, cfg)
	if got, progress, err := c.Fetch(context.Background(), digest(data), int64(len(data)), get); err != nil || !bytes.Equal(got, data) || progress.ResumedBytes != evidence.MaxChunk {
		t.Fatal(progress, err)
	}
}

func TestExplicitDiscardRepairsCorruptEntryWithoutTouchingOthers(t *testing.T) {
	cfg, data := fixture()
	path := filepath.Join(t.TempDir(), "cache")
	c := open(t, path, cfg)
	if _, _, err := c.Fetch(context.Background(), digest(data), int64(len(data)), getter(data, nil)); err != nil {
		t.Fatal(err)
	}
	other := []byte("unrelated cached object")
	if _, _, err := c.Fetch(context.Background(), digest(other), int64(len(other)), getter(other, nil)); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := os.WriteFile(filepath.Join(path, digest(data)+".json"), []byte("corrupt checkpoint"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(path); err == nil {
		t.Fatal("corrupt checkpoint accepted")
	}
	if err := Discard(path, digest(data)); err != nil {
		t.Fatal(err)
	}
	state, err := Inspect(path)
	if err != nil || len(state.Entries) != 1 || state.Entries[0].SHA256 != digest(other) {
		t.Fatal(state, err)
	}
}

func TestUnboundDirectoryNeverDiscardsExistingFiles(t *testing.T) {
	cfg, _ := fixture()
	path := t.TempDir()
	sentinel := filepath.Join(path, ".checkpoint")
	if err := os.WriteFile(sentinel, []byte("unrelated private data"), 0600); err != nil {
		t.Fatal(err)
	}
	if c, err := Open(path, cfg); err == nil {
		c.Close()
		t.Fatal("unbound nonempty directory adopted")
	}
	raw, err := os.ReadFile(sentinel)
	if err != nil || string(raw) != "unrelated private data" {
		t.Fatal("existing file changed", err)
	}
}

func TestExpiryAfterPersistenceDoesNotReturnSuccess(t *testing.T) {
	cfg, _ := fixture()
	data := []byte("small package")
	now := time.Now()
	cfg.Now = func() time.Time { return now }
	c := open(t, filepath.Join(t.TempDir(), "cache"), cfg)
	c.afterWrite = func(stage string) error {
		if stage == "after-checkpoint-sync" {
			now = now.Add(2 * time.Hour)
		}
		return nil
	}
	if got, _, err := c.Fetch(context.Background(), digest(data), int64(len(data)), getter(data, nil)); err == nil || got != nil {
		t.Fatal("expired cache result returned")
	}
}

func TestCacheCrashRecovery(t *testing.T) {
	for _, test := range []struct {
		stage  string
		count  int
		offset int64
	}{{"after-data-sync", 1, 0}, {"after-data-sync", 2, evidence.MaxChunk}, {"after-checkpoint-rename", 2, 2 * evidence.MaxChunk}, {"after-checkpoint-sync", 2, 2 * evidence.MaxChunk}} {
		t.Run(test.stage+strconv.Itoa(test.count), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cache")
			cmd := exec.Command(os.Args[0], "-test.run=^TestCacheCrashHelper$")
			cmd.Env = append(os.Environ(), "WORLD_CACHE_CRASH_PATH="+path, "WORLD_CACHE_CRASH_STAGE="+test.stage, "WORLD_CACHE_CRASH_COUNT="+strconv.Itoa(test.count))
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 73 {
				t.Fatal(string(output), err)
			}
			cfg, data := fixture()
			c := open(t, path, cfg)
			var offsets []int64
			get := getter(data, &offsets)
			// Retention is immutable. Reuse the original response deadline retained
			// by the first process, while proving reads begin at its committed offset.
			if e, ok := c.entries[digest(data)]; ok {
				base := get
				get = func(ctx context.Context, r evidence.GetRequest) (evidence.GetResponse, error) {
					v, err := base(ctx, r)
					v.RetainUntilUnixMS = e.RetainUntilUnixMS
					return v, err
				}
			}
			got, progress, err := c.Fetch(context.Background(), digest(data), int64(len(data)), get)
			if err != nil || !bytes.Equal(got, data) || offsets[0] != test.offset || progress.ResumedBytes != test.offset {
				t.Fatal(progress, offsets, err)
			}
		})
	}
}

func TestCacheCrashHelper(t *testing.T) {
	path := os.Getenv("WORLD_CACHE_CRASH_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	cfg, data := fixture()
	c, err := Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	limit, _ := strconv.Atoi(os.Getenv("WORLD_CACHE_CRASH_COUNT"))
	count := 0
	c.afterWrite = func(stage string) error {
		if stage == os.Getenv("WORLD_CACHE_CRASH_STAGE") {
			count++
			if count == limit {
				os.Exit(73)
			}
		}
		return nil
	}
	_, _, err = c.Fetch(context.Background(), digest(data), int64(len(data)), getter(data, nil))
	t.Fatal("did not crash", err)
}
