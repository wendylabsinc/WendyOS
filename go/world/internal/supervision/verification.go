package supervision

// Bounds apply to one fresh verification chunk. The task store may have up to
// 32 active supervisors and must split them into at most four such chunks.
const (
	MaxVerificationProofs     = 8
	MaxVerificationProofBytes = 128 << 10
	MaxVerificationBytes      = 1 << 20
)

// Verification is a local, stage-only result, not a wire grant or cached
// permission. InputSHA256 hashes the exact supplied bytes; Operator.Proof holds
// the canonical signed ACK. Denied results contain neither Ack nor Operator.
// Callers must match every challenge/identity binding and fixed local deadline.
type Verification struct {
	InputSHA256 string
	Denied      bool
	Ack         Ack
	Operator    Operator
}
