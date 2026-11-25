// Package supervision defines the simulation supervision handshake. It carries
// no authority or timers: signature verification and monotonic leases are owned
// by authority and tasking respectively.
package supervision

import (
	"encoding/json"
	"time"
)

const Version = 1

// MaxLease is the signed simulation wire profile limit, measured from issuance.
const MaxLease = 500 * time.Millisecond

type Challenge struct {
	Version           int    `json:"version"`
	Tenant            string `json:"tenant"`
	Site              string `json:"site"`
	Domain            string `json:"domain"`
	RunID             string `json:"run_id"`
	Audience          string `json:"audience"`
	Assignee          string `json:"assignee"`
	GatewayPrincipal  string `json:"gateway_principal"`
	GatewayBootID     string `json:"gateway_boot_id"`
	TaskID            string `json:"task_id"`
	SpecSHA256        string `json:"spec_sha256"`
	ExecutorEpoch     uint64 `json:"executor_epoch"`
	OperatorPrincipal string `json:"operator_principal"`
	OperatorKeySHA256 string `json:"operator_key_sha256"`
	OperatorGrantID   string `json:"operator_grant_id"`
	RequestID         string `json:"request_id"`
	ChallengeID       string `json:"challenge_id"`
	IssuedAtUnixMS    int64  `json:"issued_at_unix_ms"`
	ExpiresAtUnixMS   int64  `json:"expires_at_unix_ms"`
}

type Ack struct {
	Challenge Challenge `json:"challenge"`
}
type Receipt struct {
	Challenge Challenge `json:"challenge"`
	AckSHA256 string    `json:"ack_sha256"`
}

// Proof retains the signed ACK for current grant and revocation revalidation.
// Receipt arrival never renews the challenge's fixed deadline.
type Operator struct {
	Principal        string          `json:"principal"`
	KeySHA256        string          `json:"key_sha256"`
	GrantID          string          `json:"grant_id"`
	ValidUntilUnixMS int64           `json:"valid_until_unix_ms"`
	Proof            json.RawMessage `json:"proof,omitempty"`
}
