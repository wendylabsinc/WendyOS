package services

import "time"

// SetPairingProbeRetryPause shortens the pause between pairing probes in tests.
func SetPairingProbeRetryPause(d time.Duration) (restore func()) {
	old := pairingProbeRetryPause
	pairingProbeRetryPause = d
	return func() { pairingProbeRetryPause = old }
}
