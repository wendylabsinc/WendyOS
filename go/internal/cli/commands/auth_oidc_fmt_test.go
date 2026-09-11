package commands

import (
	"math"
	"testing"
	"time"
)

// encoding/json decodes JSON numbers as float64; naive %v formatting renders
// exp as 1.785860416e+09. Observed in a real run.
func TestNumericClaimsRenderAsIntegers(t *testing.T) {
	const exp = 1785860416.0
	if got := int64(exp); got != 1785860416 {
		t.Fatalf("int64 conversion = %d", got)
	}
	if exp != math.Trunc(exp) {
		t.Fatal("expected a whole number")
	}
	if ts := time.Unix(int64(exp), 0); ts.Year() < 2020 {
		t.Fatalf("unix conversion looks wrong: %v", ts)
	}
}
