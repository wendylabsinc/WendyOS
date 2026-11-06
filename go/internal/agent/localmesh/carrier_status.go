package localmesh

// CarrierStatus describes local transport setup, not peer reachability. Ready
// requires listening and discovery resources to be installed. A carrier with
// multiple interfaces can be ready and also report a partial failure.
type CarrierStatus struct {
	Ready  bool
	Detail string
	Err    error
}
