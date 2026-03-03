package hardware

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func TestSystemHardwareDiscoverer_Discover(t *testing.T) {
	logger := zap.NewNop()
	d := NewSystemHardwareDiscoverer(logger)

	caps, err := d.Discover(context.Background(), "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// On macOS, most Linux sysfs paths won't exist, so we may get zero results.
	// The test verifies that the function runs without error.
	t.Logf("Discovered %d hardware capabilities", len(caps))
}

func TestSystemHardwareDiscoverer_CategoryFilter(t *testing.T) {
	logger := zap.NewNop()
	d := NewSystemHardwareDiscoverer(logger)

	// Request only "gpu" category.
	caps, err := d.Discover(context.Background(), "gpu")
	if err != nil {
		t.Fatalf("Discover with filter: %v", err)
	}

	// Verify all returned capabilities are in the "gpu" category.
	for _, cap := range caps {
		if cap.Category != "gpu" {
			t.Errorf("expected category gpu, got %q", cap.Category)
		}
	}
}

func TestSystemHardwareDiscoverer_UnknownCategory(t *testing.T) {
	logger := zap.NewNop()
	d := NewSystemHardwareDiscoverer(logger)

	caps, err := d.Discover(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("Discover with unknown filter: %v", err)
	}

	if len(caps) != 0 {
		t.Errorf("expected 0 results for unknown category, got %d", len(caps))
	}
}

func TestSystemHardwareDiscoverer_AllCategories(t *testing.T) {
	logger := zap.NewNop()
	d := NewSystemHardwareDiscoverer(logger)

	categories := []string{"gpu", "usb", "i2c", "spi", "gpio", "camera", "audio", "network", "storage"}
	for _, cat := range categories {
		caps, err := d.Discover(context.Background(), cat)
		if err != nil {
			t.Errorf("Discover(%q): %v", cat, err)
			continue
		}
		for _, cap := range caps {
			if cap.Category != cat {
				t.Errorf("category %q: got capability with category %q", cat, cap.Category)
			}
		}
		t.Logf("  %s: %d capabilities", cat, len(caps))
	}
}
