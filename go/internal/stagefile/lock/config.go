package lock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// ConfigResolver fetches the raw OCI image-config JSON for one pinned image
// reference, as seen from one target platform. It sits beside Resolver rather
// than replacing it because the two answer different questions about the same
// image: Resolver answers "which bytes is this tag today", which is what the
// lockfile records, and ConfigResolver answers "what environment and working
// directory does that pin give a build targeting this platform", which is what
// llbgen.Emit needs and no lockfile can hold.
//
// The platform argument is not optional and not a hint. A lockfile pin is an
// image *index* digest — lock.Resolve calls crane.Digest with no platform, so
// the digest names a manifest list covering every architecture the publisher
// built. There is no single config behind such a digest, and picking the wrong
// child gives a build the wrong PATH and the wrong WORKDIR. Emit rejects a
// mismatched config with platforms.OnlyStrict, so the failure is loud rather
// than silent, but that guard is a backstop; selecting correctly here is the
// actual mechanism.
type ConfigResolver func(ref, platform string) ([]byte, error)

// configKey names one memoized lookup. Both halves belong in the key: the same
// pin resolves to a different config per platform, and a cache keyed on the ref
// alone would hand an amd64 config to an arm64 build.
type configKey struct {
	ref      string
	platform string
}

// configEntry is one lookup's in-flight-or-finished result, mirroring
// memoEntry: done is closed once config/err are written, so a caller arriving
// mid-flight waits for the first caller's answer instead of issuing a second
// identical request.
type configEntry struct {
	done   chan struct{}
	config []byte
	err    error
}

// MemoizeConfig returns a ConfigResolver that asks r for any given
// (ref, platform) pair at most once. It is Memoize's counterpart and follows
// the same two rules for the same reasons: deduplication covers in-flight
// lookups, because several Stagefiles compile concurrently in one process and
// typically share a base image; and failures are not cached, so a network blip
// does not poison the pair for the rest of a `wendy watch` session.
func MemoizeConfig(r ConfigResolver) ConfigResolver {
	var mu sync.Mutex
	entries := map[configKey]*configEntry{}

	return func(ref, platform string) ([]byte, error) {
		k := configKey{ref: ref, platform: platform}

		mu.Lock()
		if e, ok := entries[k]; ok {
			mu.Unlock()
			<-e.done
			return e.config, e.err
		}
		e := &configEntry{done: make(chan struct{})}
		entries[k] = e
		mu.Unlock()

		e.config, e.err = r(ref, platform)
		close(e.done)

		if e.err != nil {
			mu.Lock()
			// Guard the identity check: a retry that already replaced this
			// entry must not have its own lookup dropped from the map.
			if entries[k] == e {
				delete(entries, k)
			}
			mu.Unlock()
		}
		return e.config, e.err
	}
}

// ResolveConfigs fetches the image config for every ref in refs, at platform,
// and returns them keyed by ref — the same key llbgen.Emit looks them up under.
//
// Each ref is resolved at the digest images pins it to, not at its floating
// tag: the definition being compiled already names that digest, and re-reading
// the tag would let a registry push between the two lookups hand the build a
// config belonging to a different image than the one it pulls.
//
// A ref with no pin is an error rather than a skipped entry. Emit refuses a
// missing config, so a partial map only moves the same failure further from its
// cause.
func ResolveConfigs(refs []string, images map[string]string, platform string, r ConfigResolver) (map[string][]byte, error) {
	// Collect in declaration order, without duplicates, before issuing
	// anything — the same shape Resolve uses, and for the same reason: the
	// error reported must be keyed to the source file rather than to whichever
	// lookup happened to finish first.
	var pending []string
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		pending = append(pending, ref)
	}

	pinned := make([]string, len(pending))
	for i, ref := range pending {
		digest, ok := images[ref]
		if !ok {
			return nil, fmt.Errorf("no resolved digest for %q; run `stagefile lock`", ref)
		}
		pinned[i] = ref + "@" + digest
	}

	configs := make([][]byte, len(pending))
	errs := make([]error, len(pending))
	sem := make(chan struct{}, maxResolveConcurrency)
	var wg sync.WaitGroup
	for i := range pending {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			configs[i], errs[i] = r(pinned[i], platform)
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("resolving image config for %q: %w", pending[i], err)
		}
	}

	result := make(map[string][]byte, len(pending))
	for i, ref := range pending {
		result[ref] = configs[i]
	}
	return result, nil
}

// CraneConfigResolver is a ConfigResolver backed by a real registry lookup, and
// is CraneResolver's counterpart: same client, same ambient docker-config
// credentials, same reason for having no automated test (the suite never
// depends on network access; tests inject a fake).
//
// The platform is passed to crane explicitly on every call. crane defaults to
// linux/amd64 when none is given, so an omitted platform would not fail — it
// would quietly resolve the wrong child of the index, which is the one outcome
// this function exists to prevent.
func CraneConfigResolver(ref, platform string) ([]byte, error) {
	p, err := v1.ParsePlatform(platform)
	if err != nil {
		return nil, fmt.Errorf("platform %q: %w", platform, err)
	}
	if p.OS == "" || p.Architecture == "" {
		return nil, fmt.Errorf("platform %q must name both an OS and an architecture", platform)
	}

	// The same 30s bound CraneResolver uses: config resolution runs inline
	// inside every build, and a stalled connection would otherwise hang it
	// forever.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dt, err := crane.Config(ref, crane.WithContext(ctx), crane.WithPlatform(p))
	if err != nil {
		return nil, fmt.Errorf("crane config %q for %s: %w", ref, platform, err)
	}
	return dt, nil
}
