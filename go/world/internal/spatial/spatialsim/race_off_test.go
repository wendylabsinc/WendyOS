//go:build !race

package spatialsim

// raceSlowdown scales wall-clock budgets under the race detector.
const raceSlowdown = 1
