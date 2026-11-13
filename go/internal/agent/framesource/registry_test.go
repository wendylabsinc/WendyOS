package framesource

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// stubSource is a Source that only ever answers a listing.
type stubSource struct{ desc *agentpbv2.CalibratedSource }

func (s stubSource) Describe(context.Context) (*agentpbv2.CalibratedSource, error) {
	return s.desc, nil
}
func (s stubSource) Open(context.Context, Options) (Stream, error) {
	return nil, errors.New("not opened in these tests")
}

func source(name string, provides ...agentpbv2.FrameRequirement) stubSource {
	return stubSource{&agentpbv2.CalibratedSource{Source: name, Provides: provides, Available: true}}
}

func registryOf(sources ...Source) *Registry {
	return NewRegistry(ProviderFunc(func(context.Context) ([]Source, error) { return sources, nil }))
}

func TestRegistry_DescribeIsSortedSoAListingIsStable(t *testing.T) {
	reg := registryOf(source("b"), source("a"), source("c"))
	got, err := reg.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].GetSource() != "a" || got[2].GetSource() != "c" {
		t.Errorf("listing = %v", got)
	}
}

// One broken provider must not hide the cameras that work: an operator
// debugging one is still entitled to see the others.
func TestRegistry_AProviderThatFailsDoesNotHideTheOthers(t *testing.T) {
	reg := NewRegistry(
		ProviderFunc(func(context.Context) ([]Source, error) { return nil, errors.New("boom") }),
		ProviderFunc(func(context.Context) ([]Source, error) { return []Source{source("a")}, nil }),
	)
	got, err := reg.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(got) != 1 || got[0].GetSource() != "a" {
		t.Errorf("listing = %v", got)
	}
}

func TestRegistry_LookupByNameFindsAnUnavailableSourceToo(t *testing.T) {
	unavailable := stubSource{&agentpbv2.CalibratedSource{Source: "realsense", Available: false}}
	reg := registryOf(unavailable)
	// Resolving it is what lets the caller produce "the helper is not
	// installed" instead of "no such camera".
	_, desc, err := reg.Lookup(context.Background(), "realsense", nil)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if desc.GetAvailable() {
		t.Error("descriptor claims availability")
	}
}

func TestRegistry_UnknownNameListsWhatTheDeviceHas(t *testing.T) {
	reg := registryOf(source("a"), source("b"))
	_, _, err := reg.Lookup(context.Background(), "z", nil)
	if !errors.Is(err, ErrNoSuchSource) {
		t.Fatalf("err = %v, want ErrNoSuchSource", err)
	}
	if !strings.Contains(err.Error(), "a, b") {
		t.Errorf("error does not list the known sources: %v", err)
	}
}

// Picking a camera when several could serve is a coin toss over which sensor an
// app measures the world with.
func TestRegistry_AmbiguousDefaultIsRefusedNotGuessed(t *testing.T) {
	reg := registryOf(source("a", alignedDepth), source("b", alignedDepth))
	_, _, err := reg.Lookup(context.Background(), "",
		[]agentpbv2.FrameRequirement{alignedDepth})
	if !errors.Is(err, ErrAmbiguousSource) {
		t.Fatalf("err = %v, want ErrAmbiguousSource", err)
	}
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Errorf("error does not name the candidates: %v", err)
	}
}

func TestRegistry_DefaultPicksTheOnlySourceThatCanServe(t *testing.T) {
	reg := registryOf(source("colour-only"), source("depth", alignedDepth))
	_, desc, err := reg.Lookup(context.Background(), "",
		[]agentpbv2.FrameRequirement{alignedDepth})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if desc.GetSource() != "depth" {
		t.Errorf("chose %q", desc.GetSource())
	}
}

// With one camera that cannot serve, handing it back lets the caller say WHICH
// property is missing — far more useful than "no source found".
func TestRegistry_SingleIneligibleSourceIsReturnedSoTheRefusalCanBeSpecific(t *testing.T) {
	reg := registryOf(source("colour-only"))
	_, desc, err := reg.Lookup(context.Background(), "",
		[]agentpbv2.FrameRequirement{alignedDepth})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if desc.GetSource() != "colour-only" {
		t.Errorf("chose %q", desc.GetSource())
	}
}
