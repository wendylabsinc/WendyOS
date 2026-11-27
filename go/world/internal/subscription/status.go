package subscription

import "time"

type PeerStatus struct {
	Audience           string     `json:"audience"`
	LastAttempt        time.Time  `json:"last_attempt"`
	LastCompleted      *time.Time `json:"last_completed,omitempty"`
	LastOutcome        string     `json:"last_outcome"`
	SourceScopeKnown   bool       `json:"source_scope_known"`
	SourcePartial      bool       `json:"source_partial"`
	SourcePolicySHA256 string     `json:"source_policy_sha256,omitempty"`
}

type Status struct {
	RetainedFences         int          `json:"retained_fences,omitempty"`
	UnknownLocationRecords int          `json:"unknown_location_records,omitempty"`
	Version                int          `json:"version"`
	Tenant                 string       `json:"tenant"`
	Site                   string       `json:"site"`
	Mode                   string       `json:"mode"`
	Policy                 *Policy      `json:"policy,omitempty"`
	PolicySHA256           string       `json:"policy_sha256,omitempty"`
	RetainedRecords        int          `json:"retained_records"`
	RetainedTombstones     int          `json:"retained_tombstones"`
	ExpiredRecords         int          `json:"expired_records"`
	LimitRecords           int          `json:"limit_records"`
	ObservedAt             time.Time    `json:"observed_at"`
	Peers                  []PeerStatus `json:"peers"`
	SyncSemantics          string       `json:"sync_semantics"`
}
