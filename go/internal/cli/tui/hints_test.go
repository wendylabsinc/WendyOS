package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestNewHintRotatorPicksHintFromList(t *testing.T) {
	r := newHintRotator()
	if r.current == "" {
		t.Fatal("expected newHintRotator to pick a non-empty starting hint")
	}
	found := false
	for _, h := range ProgressHints {
		if h == r.current {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("starting hint %q is not in ProgressHints", r.current)
	}
}

func TestHintRotatorNextAvoidsImmediateRepeat(t *testing.T) {
	r := hintRotator{hints: []string{"a", "b", "c"}, current: "a"}
	prev := r.current
	for i := 0; i < 50; i++ {
		r.next()
		if r.current == prev {
			t.Fatalf("next() repeated hint %q on iteration %d", r.current, i)
		}
		prev = r.current
	}
}

func TestHintRotatorNextNoopForSingleHint(t *testing.T) {
	r := hintRotator{hints: []string{"only"}, current: "only"}
	r.next()
	if r.current != "only" {
		t.Fatalf("expected single-hint rotator to stay %q, got %q", "only", r.current)
	}
}

func TestHintRotatorNextNoopForEmpty(t *testing.T) {
	r := hintRotator{} // must not panic
	r.next()
	if r.current != "" {
		t.Fatalf("expected empty rotator to stay empty, got %q", r.current)
	}
}

func TestHintRotatorViewRendersHint(t *testing.T) {
	r := hintRotator{hints: []string{"do a thing"}, current: "do a thing"}
	v := r.view(0)
	if !strings.Contains(v, "do a thing") {
		t.Fatalf("view %q does not contain the hint text", v)
	}
	if !strings.Contains(v, "💡") {
		t.Fatalf("view %q does not contain the hint marker", v)
	}
}

func TestHintRotatorViewEmptyForNoHint(t *testing.T) {
	r := hintRotator{}
	if v := r.view(0); v != "" {
		t.Fatalf("expected empty view for empty rotator, got %q", v)
	}
	if r.tick() != nil {
		t.Fatal("empty rotator scheduled a tick")
	}
}

// TestHintRotatorViewTruncatesToWidth verifies that a hint wider than the given
// terminal width is truncated so its display width never exceeds that width.
// Without this, the terminal would soft-wrap the hint onto a second physical
// row that Bubble Tea does not count, desyncing its in-place frame redraw and
// leaving garbled/duplicated spinner lines.
func TestHintRotatorViewTruncatesToWidth(t *testing.T) {
	long := "Grant hardware access (GPU, camera, GPIO) via entitlements in wendy.json"
	r := hintRotator{hints: []string{long}, current: long}

	const width = 30
	v := r.view(width)
	if got := ansi.StringWidth(v); got > width {
		t.Fatalf("truncated hint width = %d, want <= %d (view=%q)", got, width, v)
	}
	// The rendered line must still be recognizably a hint and must have been
	// shortened (an ellipsis tail is appended on truncation).
	if !strings.Contains(v, "💡") {
		t.Fatalf("truncated view %q lost the hint marker", v)
	}
	if !strings.Contains(v, "…") {
		t.Fatalf("expected truncated view %q to end with an ellipsis", v)
	}
}

// TestHintRotatorViewLeavesShortHintIntact verifies that a hint narrower than
// the given width is not truncated (no ellipsis, full text preserved), and that
// width 0 (no WindowSizeMsg yet) also leaves it intact.
func TestHintRotatorViewLeavesShortHintIntact(t *testing.T) {
	short := "short tip"
	r := hintRotator{hints: []string{short}, current: short}

	for _, width := range []int{0, 80} {
		v := r.view(width)
		if !strings.Contains(v, short) {
			t.Fatalf("width %d: view %q dropped the hint text", width, v)
		}
		if strings.Contains(v, "…") {
			t.Fatalf("width %d: short hint %q was unexpectedly truncated", width, v)
		}
	}
}
