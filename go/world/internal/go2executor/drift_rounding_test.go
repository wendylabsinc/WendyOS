package go2executor

import (
	"context"
	"math"
	"strings"
	"testing"
)

// Expected bits come from the existing Python bridge expression: separately
// rounded binary64 products and left-associated additions, not a fused FMA.
func TestOdometryMinimumPythonBinary64Vectors(t *testing.T) {
	cases := []struct {
		age           float64
		position, yaw uint64
	}{
		{0, 0x3fb1eb851eb851ec, 0x3f947ae147ae147b},
		{math.SmallestNonzeroFloat64, 0x3fb1eb851eb851ec, 0x3f947ae147ae147b},
		{.001, 0x3fb1eb97e0a3b0de, 0x3f947aeeb3a761d9},
		{1, 0x3fb234ca9e233305, 0x3f94af4f0d844d01},
		{12.128000000000718, 0x3fb564280d6a584e, 0x3f96f6bc8a5f6a1c},
		{12.12800000000072, 0x3fb564280d6a584e, 0x3f96f6bc8a5f6a1c},
		{12.128000000000721, 0x3fb564280d6a584e, 0x3f96f6bc8a5f6a1d},
		{119.99999999999999, 0x3fca220c6c68eadf, 0x3fa6872b020c49ba},
		{120, 0x3fca220c6c68eadf, 0x3fa6872b020c49ba},
	}
	for _, tc := range cases {
		p, y := odometryMinimum(.02, .02, .05, tc.age)
		if math.Float64bits(p) != tc.position || math.Float64bits(y) != tc.yaw {
			t.Fatalf("age %.17g: got %016x/%016x want %016x/%016x", tc.age, math.Float64bits(p), math.Float64bits(y), tc.position, tc.yaw)
		}
	}
	// K's original strict comparison failed on an FMA-capable host; make the
	// regression discriminate between the two arithmetic rules on every host.
	if math.Float64bits(math.FMA(.0002, 12.12800000000072, .02)) != 0x3f96f6bc8a5f6a1d {
		t.Fatal("K fixture no longer distinguishes fused arithmetic")
	}
}

func TestKDriftBudgetExactAndOneULPBelow(t *testing.T) {
	for _, name := range []string{"exact", "position-one-ulp-below", "yaw-one-ulp-below"} {
		t.Run(name, func(t *testing.T) {
			c, _, a, _ := fixture(t)
			if err := c.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			s, err := a.State(context.Background(), c.session)
			if err != nil {
				t.Fatal(err)
			}
			// Exact public K capture values. Other lifecycle fields use the independent
			// fixture so this test exercises the real validator without stale evidence.
			s.OdometryEpochAgeS = 12.12800000000072
			s.Scan.PositionUncertaintyM = 0.08355951621555954
			s.Scan.YawUncertaintyRad = 0.022425600000000143
			switch name {
			case "position-one-ulp-below":
				s.Scan.PositionUncertaintyM = math.Nextafter(s.Scan.PositionUncertaintyM, math.Inf(-1))
			case "yaw-one-ulp-below":
				s.Scan.YawUncertaintyRad = math.Nextafter(s.Scan.YawUncertaintyRad, math.Inf(-1))
			}
			err = c.validateState(s)
			if name == "exact" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "omits commissioned drift budget") {
				t.Fatalf("strict under-budget rejection missing: %v", err)
			}
			if len(a.commands) != 0 {
				t.Fatal("validation issued a command")
			}
		})
	}
}
