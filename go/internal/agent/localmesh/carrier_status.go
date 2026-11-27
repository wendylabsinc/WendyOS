package localmesh

import "errors"

// ErrNoCarrierHardware means this host has no compatible hardware for a
// carrier. The supervisor can leave it idle for this configuration lifetime.
var ErrNoCarrierHardware = errors.New("no compatible hardware")

// CarrierStatus describes local transport setup, not peer reachability. Ready
// requires listening and discovery resources to be installed. A carrier with
// multiple interfaces can be ready and also report a partial failure.
type CarrierStatus struct {
	Ready      bool
	NoHardware bool
	Waiting    bool
	Detail     string
	Err        error
}
