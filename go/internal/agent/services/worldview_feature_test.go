package services

import (
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/worldview"
)

func TestFeaturesetWorldViewFollowsRuntimeSupport(t *testing.T) {
	for _, supported := range []bool{true, false} {
		got := appendWorldViewFeature([]string{"camera"}, supported)
		if slices.Contains(got, "world-view") != supported || !slices.Contains(got, "camera") {
			t.Fatalf("supported=%v: features %v", supported, got)
		}
	}
}

func TestFeaturesetDetectAdvertisesWorldView(t *testing.T) {
	prev := worldViewFeatureSupported
	t.Cleanup(func() { worldViewFeatureSupported = prev })

	for _, supported := range []bool{true, false} {
		worldViewFeatureSupported = func() bool { return supported }
		got := detectFeatureset()
		n := 0
		for _, f := range got {
			if f == "world-view" {
				n++
			}
		}
		want := 0
		if supported {
			want = 1
		}
		if n != want {
			t.Fatalf("supported=%v: world-view appears %d times in %v", supported, n, got)
		}
	}
}

// The real seam is wired to the runtime's own answer, not a copy of its table.
func TestFeaturesetWorldViewUsesRuntimeSupported(t *testing.T) {
	if got, want := worldViewFeatureSupported(), worldview.Supported(); got != want {
		t.Fatalf("worldViewFeatureSupported() = %v, worldview.Supported() = %v", got, want)
	}
	got := detectFeatureset()
	if slices.Contains(got, "world-view") != worldview.Supported() {
		t.Fatalf("world-view presence %v, worldview.Supported() %v: %v",
			slices.Contains(got, "world-view"), worldview.Supported(), got)
	}
}
