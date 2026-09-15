package lock

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConfigResolver answers from a map keyed by the exact (ref, platform)
// pair it is asked for, so a test can assert both halves of the request rather
// than only that something came back.
func fakeConfigResolver(answers map[string]string) ConfigResolver {
	return func(ref, platform string) ([]byte, error) {
		body, ok := answers[ref+" "+platform]
		if !ok {
			return nil, fmt.Errorf("no fake answer for %q at %q", ref, platform)
		}
		return []byte(body), nil
	}
}

func TestResolveConfigsAsksForThePinnedDigestAtTheTargetPlatform(t *testing.T) {
	images := map[string]string{"python:3.12-slim": "sha256:aaa"}
	r := fakeConfigResolver(map[string]string{
		"python:3.12-slim@sha256:aaa linux/arm64": `{"os":"linux"}`,
	})

	got, err := ResolveConfigs([]string{"python:3.12-slim"}, images, "linux/arm64", r)
	if err != nil {
		t.Fatalf("ResolveConfigs: %v", err)
	}
	// Keyed by the plain ref, because that is the key llbgen.Emit looks the
	// config up under.
	if string(got["python:3.12-slim"]) != `{"os":"linux"}` {
		t.Fatalf("configs = %v", got)
	}
}

// The lockfile pins a multi-arch index digest, so the same pin must be able to
// yield a different config per platform. A resolver that ignored the platform
// would return the index's default arch and Emit would reject it.
func TestResolveConfigsIsPerPlatform(t *testing.T) {
	images := map[string]string{"debian:12": "sha256:bbb"}
	r := fakeConfigResolver(map[string]string{
		"debian:12@sha256:bbb linux/arm64": `{"architecture":"arm64"}`,
		"debian:12@sha256:bbb linux/amd64": `{"architecture":"amd64"}`,
	})

	for _, platform := range []string{"linux/arm64", "linux/amd64"} {
		got, err := ResolveConfigs([]string{"debian:12"}, images, platform, r)
		if err != nil {
			t.Fatalf("ResolveConfigs(%s): %v", platform, err)
		}
		want := `{"architecture":"` + strings.TrimPrefix(platform, "linux/") + `"}`
		if string(got["debian:12"]) != want {
			t.Fatalf("configs[%s] = %s, want %s", platform, got["debian:12"], want)
		}
	}
}

func TestResolveConfigsRequiresAPinForEveryRef(t *testing.T) {
	r := fakeConfigResolver(nil)
	_, err := ResolveConfigs([]string{"debian:12"}, map[string]string{}, "linux/arm64", r)
	if err == nil {
		t.Fatal("expected an error for an unpinned ref")
	}
	if !strings.Contains(err.Error(), "debian:12") {
		t.Fatalf("error should name the unpinned ref, got %v", err)
	}
}

// Two refs failing at once must always report the same one, or the same broken
// Stagefile produces a different error depending on registry timing.
func TestResolveConfigsReportsTheFirstFailureInDeclarationOrder(t *testing.T) {
	images := map[string]string{"a:1": "sha256:a", "b:1": "sha256:b"}
	r := func(ref, platform string) ([]byte, error) {
		return nil, errors.New("boom " + ref)
	}
	for range 20 {
		_, err := ResolveConfigs([]string{"a:1", "b:1"}, images, "linux/arm64", r)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "a:1") {
			t.Fatalf("expected the first ref's failure, got %v", err)
		}
	}
}

func TestResolveConfigsDeduplicatesRepeatedRefs(t *testing.T) {
	var calls atomic.Int64
	images := map[string]string{"a:1": "sha256:a"}
	r := func(ref, platform string) ([]byte, error) {
		calls.Add(1)
		return []byte(`{}`), nil
	}
	if _, err := ResolveConfigs([]string{"a:1", "a:1", "a:1"}, images, "linux/arm64", r); err != nil {
		t.Fatalf("ResolveConfigs: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver called %d times, want 1", got)
	}
}

func TestMemoizeConfigAsksOncePerRefAndPlatform(t *testing.T) {
	var calls atomic.Int64
	r := MemoizeConfig(func(ref, platform string) ([]byte, error) {
		calls.Add(1)
		return []byte(ref + " " + platform), nil
	})

	for range 3 {
		if _, err := r("a:1", "linux/arm64"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver called %d times, want 1", got)
	}

	// A second platform is a different image config, not a cache hit.
	got, err := r("a:1", "linux/amd64")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if string(got) != "a:1 linux/amd64" {
		t.Fatalf("config = %s", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("resolver called %d times, want 2", got)
	}
}

// A network blip must not poison the ref for the rest of a long-lived
// `wendy watch` session — the same rule Memoize follows for digests.
func TestMemoizeConfigDoesNotCacheFailures(t *testing.T) {
	var calls atomic.Int64
	r := MemoizeConfig(func(ref, platform string) ([]byte, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("blip")
		}
		return []byte(`{}`), nil
	})

	if _, err := r("a:1", "linux/arm64"); err == nil {
		t.Fatal("expected the first call to fail")
	}
	if _, err := r("a:1", "linux/arm64"); err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
}

func TestMemoizeConfigCollapsesInFlightLookups(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	r := MemoizeConfig(func(ref, platform string) ([]byte, error) {
		calls.Add(1)
		<-release
		return []byte(`{}`), nil
	})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r("a:1", "linux/arm64"); err != nil {
				t.Errorf("resolve: %v", err)
			}
		}()
	}
	// Give the goroutines time to pile up on the same in-flight entry.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver called %d times, want 1", got)
	}
}
