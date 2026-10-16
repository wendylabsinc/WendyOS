package framesource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// isCleanEnd reports whether a record read ended because the producer finished,
// rather than because something went wrong. A truncated record is not a clean
// end — see ReadRecord.
func isCleanEnd(err error) bool { return errors.Is(err, io.EOF) }

// Provider enumerates the sources of one capture technology. Sources is called
// per listing and per subscribe, so it must be cheap and must not disturb a
// capture already running.
type Provider interface {
	Sources(ctx context.Context) ([]Source, error)
}

// ProviderFunc adapts a function to Provider.
type ProviderFunc func(ctx context.Context) ([]Source, error)

func (f ProviderFunc) Sources(ctx context.Context) ([]Source, error) { return f(ctx) }

// Registry is every calibrated-frame source this agent can offer.
type Registry struct {
	providers []Provider
}

// NewRegistry builds a registry over the given providers, in order. Earlier
// providers win a name collision, which never happens in practice because each
// provider prefixes its source ids with its own kind.
func NewRegistry(providers ...Provider) *Registry {
	return &Registry{providers: providers}
}

// Sources enumerates every source. A provider that fails does not hide the
// ones that did not: an operator debugging one camera must still be able to see
// the others.
func (r *Registry) Sources(ctx context.Context) ([]Source, error) {
	var (
		out    []Source
		seen   = map[string]bool{}
		errs   []string
		anyErr bool
	)
	for _, p := range r.providers {
		sources, err := p.Sources(ctx)
		if err != nil {
			anyErr = true
			errs = append(errs, err.Error())
			continue
		}
		for _, s := range sources {
			desc, err := s.Describe(ctx)
			if err != nil {
				anyErr = true
				errs = append(errs, err.Error())
				continue
			}
			if name := desc.GetSource(); name != "" && !seen[name] {
				seen[name] = true
				out = append(out, s)
			}
		}
	}
	if anyErr && len(out) == 0 {
		return nil, fmt.Errorf("enumerating calibrated frame sources: %s", strings.Join(errs, "; "))
	}
	return out, nil
}

// Describe renders every source as its proto descriptor, sorted by name so a
// listing is stable between calls.
func (r *Registry) Describe(ctx context.Context) ([]*agentpbv2.CalibratedSource, error) {
	sources, err := r.Sources(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*agentpbv2.CalibratedSource, 0, len(sources))
	for _, s := range sources {
		desc, err := s.Describe(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, desc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetSource() < out[j].GetSource() })
	return out, nil
}

// ErrNoSuchSource is returned when a name matches nothing.
var ErrNoSuchSource = errors.New("no such calibrated frame source")

// ErrAmbiguousSource is returned when a request named no source and more than
// one could serve it. Picking one would be a coin toss over which camera an app
// measures the world with.
var ErrAmbiguousSource = errors.New("more than one calibrated frame source")

// Lookup resolves a name to a source. An empty name selects the only source
// that satisfies require — which is the common case (one depth camera) and is
// refused rather than guessed when it is not.
func (r *Registry) Lookup(ctx context.Context, name string, require []agentpbv2.FrameRequirement) (Source, *agentpbv2.CalibratedSource, error) {
	sources, err := r.Sources(ctx)
	if err != nil {
		return nil, nil, err
	}
	type candidate struct {
		src  Source
		desc *agentpbv2.CalibratedSource
	}
	var (
		all       []candidate
		eligible  []candidate
		knownName []string
	)
	for _, s := range sources {
		desc, err := s.Describe(ctx)
		if err != nil {
			return nil, nil, err
		}
		all = append(all, candidate{s, desc})
		knownName = append(knownName, desc.GetSource())
		if desc.GetAvailable() && len(MissingFromSource(desc, require)) == 0 {
			eligible = append(eligible, candidate{s, desc})
		}
	}
	if name != "" {
		for _, c := range all {
			if c.desc.GetSource() == name {
				return c.src, c.desc, nil
			}
		}
		sort.Strings(knownName)
		if len(knownName) == 0 {
			return nil, nil, fmt.Errorf("%w: %q; this device has no calibrated frame sources", ErrNoSuchSource, name)
		}
		return nil, nil, fmt.Errorf("%w: %q; this device has %s", ErrNoSuchSource, name, strings.Join(knownName, ", "))
	}
	switch len(eligible) {
	case 1:
		return eligible[0].src, eligible[0].desc, nil
	case 0:
		// Nothing eligible: hand back the single source anyway so the caller
		// produces a requirement refusal naming what is missing, which is far
		// more useful than "no source found".
		if len(all) == 1 {
			return all[0].src, all[0].desc, nil
		}
		if len(all) == 0 {
			return nil, nil, fmt.Errorf("%w: this device has no calibrated frame sources", ErrNoSuchSource)
		}
		sort.Strings(knownName)
		return nil, nil, fmt.Errorf("%w: none of %s can provide %s; name one with --source to see why",
			ErrNoSuchSource, strings.Join(knownName, ", "), Slugs(Normalise(require)))
	default:
		names := make([]string, 0, len(eligible))
		for _, c := range eligible {
			names = append(names, c.desc.GetSource())
		}
		sort.Strings(names)
		return nil, nil, fmt.Errorf("%w could serve this request (%s); name one",
			ErrAmbiguousSource, strings.Join(names, ", "))
	}
}
