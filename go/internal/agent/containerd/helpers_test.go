package containerd

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func TestComputeChainID_FirstLayer(t *testing.T) {
	diffID := "sha256:abc123"
	got := computeChainID("", diffID)
	if got != diffID {
		t.Errorf("computeChainID(\"\", %q) = %q; want %q", diffID, got, diffID)
	}
}

func TestComputeChainID_WithParent(t *testing.T) {
	parent := "sha256:aaaa"
	diffID := "sha256:bbbb"

	h := sha256.New()
	h.Write([]byte(parent + " " + diffID))
	expected := fmt.Sprintf("sha256:%x", h.Sum(nil))

	got := computeChainID(parent, diffID)
	if got != expected {
		t.Errorf("computeChainID(%q, %q) = %q; want %q", parent, diffID, got, expected)
	}
}

func TestComputeChainID_Chained(t *testing.T) {
	// Simulate chaining three layers.
	diff0 := "sha256:layer0"
	diff1 := "sha256:layer1"
	diff2 := "sha256:layer2"

	chain0 := computeChainID("", diff0)
	if chain0 != diff0 {
		t.Fatalf("chain0 should equal diff0")
	}

	chain1 := computeChainID(chain0, diff1)
	chain2 := computeChainID(chain1, diff2)

	// Verify chain2 is deterministic.
	chain2Again := computeChainID(computeChainID(diff0, diff1), diff2)
	if chain2 != chain2Again {
		t.Errorf("chaining is not deterministic: %q != %q", chain2, chain2Again)
	}

	// Verify it has the sha256: prefix.
	if !strings.HasPrefix(chain2, "sha256:") {
		t.Errorf("chain ID should have sha256: prefix, got %q", chain2)
	}
}

func TestParseRestartPolicyLabel_Simple(t *testing.T) {
	tests := []struct {
		label      string
		wantPolicy string
		wantRetry  int
	}{
		{"no", "no", 0},
		{"unless-stopped", "unless-stopped", 0},
		{"on-failure", "on-failure", 0},
		{"on-failure:5", "on-failure", 5},
		{"on-failure:0", "on-failure", 0},
		{"on-failure:100", "on-failure", 100},
		{"on-failure:abc", "on-failure", 0}, // invalid number falls back to 0
		{"", "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			policy, maxRetries := parseRestartPolicyLabel(tt.label)
			if policy != tt.wantPolicy {
				t.Errorf("policy = %q; want %q", policy, tt.wantPolicy)
			}
			if maxRetries != tt.wantRetry {
				t.Errorf("maxRetries = %d; want %d", maxRetries, tt.wantRetry)
			}
		})
	}
}

func TestGCTimestamp_ValidRFC3339(t *testing.T) {
	ts := gcTimestamp()
	if ts == "" {
		t.Fatal("gcTimestamp returned empty string")
	}

	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("gcTimestamp returned invalid RFC3339: %q: %v", ts, err)
	}

	// Should be within the last few seconds.
	diff := time.Since(parsed)
	if diff < 0 || diff > 5*time.Second {
		t.Errorf("gcTimestamp is not recent (diff = %v)", diff)
	}
}

func TestGCTimestamp_IsUTC(t *testing.T) {
	ts := gcTimestamp()
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Location() != time.UTC {
		t.Errorf("gcTimestamp should be UTC, got %v", parsed.Location())
	}
}

func TestWendyLabels_Basic(t *testing.T) {
	labels := wendyLabels("myapp", "1.0.0", nil)

	if v, ok := labels[labelKeyAppVersion]; !ok {
		t.Error("missing app version label")
	} else if v != "1.0.0" {
		t.Errorf("app version = %q; want %q", v, "1.0.0")
	}

	// No restart policy should mean no restart policy label.
	if _, ok := labels[labelKeyRestartPolicy]; ok {
		t.Error("should not have restart policy label when policy is nil")
	}
}

func TestWendyLabels_WithRestartPolicyUnlessStopped(t *testing.T) {
	rp := &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED}
	labels := wendyLabels("app", "2.0", rp)

	if v, ok := labels[labelKeyRestartPolicy]; !ok {
		t.Error("missing restart policy label")
	} else if v != "unless-stopped" {
		t.Errorf("restart policy = %q; want %q", v, "unless-stopped")
	}
}

func TestWendyLabels_WithRestartPolicyOnFailure(t *testing.T) {
	rp := &agentpb.RestartPolicy{
		Mode:                agentpb.RestartPolicyMode_ON_FAILURE,
		OnFailureMaxRetries: 3,
	}
	labels := wendyLabels("app", "1.0", rp)

	if v := labels[labelKeyRestartPolicy]; v != "on-failure:3" {
		t.Errorf("restart policy = %q; want %q", v, "on-failure:3")
	}
}

func TestWendyLabels_WithRestartPolicyNo(t *testing.T) {
	rp := &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_NO}
	labels := wendyLabels("app", "1.0", rp)

	if v := labels[labelKeyRestartPolicy]; v != "no" {
		t.Errorf("restart policy = %q; want %q", v, "no")
	}
}

func TestWendyLabels_WithRestartPolicyDefault(t *testing.T) {
	rp := &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_DEFAULT}
	labels := wendyLabels("app", "1.0", rp)

	if v := labels[labelKeyRestartPolicy]; v != "unless-stopped" {
		t.Errorf("restart policy = %q; want %q (DEFAULT maps to unless-stopped)", v, "unless-stopped")
	}
}

func TestRestartPolicyToLabel_Nil(t *testing.T) {
	got := restartPolicyToLabel(nil)
	if got != "" {
		t.Errorf("restartPolicyToLabel(nil) = %q; want empty", got)
	}
}

func TestRestartPolicyToLabel_OnFailureNoRetries(t *testing.T) {
	rp := &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_ON_FAILURE}
	got := restartPolicyToLabel(rp)
	if got != "on-failure" {
		t.Errorf("restartPolicyToLabel = %q; want %q", got, "on-failure")
	}
}
