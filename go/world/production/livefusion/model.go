package livefusion

import (
	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
)

type ProofReceipt struct {
	Reference    proofref.Reference `json:"reference"`
	EnvelopeJSON []byte             `json:"envelope_json"`
}
type MediaReceipt struct {
	Source    serviceaccess.Source `json:"source"`
	Reference sensing.EvidenceRef  `json:"reference"`
	Data      []byte               `json:"data"`
}
type Result struct {
	Version          int            `json:"version"`
	RunID            string         `json:"run_id"`
	Status           string         `json:"status"`
	ErrorCode        string         `json:"error,omitempty"`
	FailureStage     string         `json:"failure_stage,omitempty"`
	Attempts         int            `json:"attempts"`
	Evaluation       *fusion.Result `json:"evaluation,omitempty"`
	Inputs           []ProofReceipt `json:"inputs,omitempty"`
	Media            []MediaReceipt `json:"media,omitempty"`
	RequestJSON      []byte         `json:"request_json,omitempty"`
	SignedRecordJSON []byte         `json:"signed_record_json,omitempty"`
	RecordSHA256     string         `json:"record_sha256,omitempty"`
	ProofSHA256      string         `json:"proof_sha256,omitempty"`
	Diagnostics      *Diagnostics   `json:"diagnostics,omitempty"`
	LocallyVerified  bool           `json:"locally_verified"`
	InputAuthority   string         `json:"input_authority,omitempty"`
}
