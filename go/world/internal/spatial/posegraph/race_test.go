//go:build race

package posegraph

// raceEnabled reports a build with the race detector, which slows the
// factorisation about twentyfold and voids wall-clock budgets.
const raceEnabled = true
