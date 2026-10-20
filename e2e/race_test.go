//go:build e2e && race

package e2e

// raceEnabled mirrors whether the tests run with -race, so the binary under
// test is built the same way.
const raceEnabled = true
