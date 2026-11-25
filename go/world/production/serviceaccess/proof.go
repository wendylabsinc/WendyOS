package serviceaccess

// RecordProof carries one exact historical envelope from an authenticated,
// source-scoped read. The RPC must label it historical; its presence never
// establishes current publication authority or live-state freshness.
type RecordProof struct {
	Envelope          []byte
	RetainUntilUnixMS int64
}
