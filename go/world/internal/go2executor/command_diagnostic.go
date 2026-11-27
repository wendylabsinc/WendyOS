package go2executor

import (
	"errors"
	"time"
)

// BridgeCommandAcknowledgment records the bridge's completed local DDS write.
// It does not identify a receiver acceptance or prove actuator response. The
// source timestamp may deliberately precede publication to enforce expiry.
type BridgeCommandAcknowledgment struct {
	Accepted          bool   `json:"accepted"`
	SourceTimestampNS int64  `json:"source_timestamp_ns"`
	NotAfterUnixMS    int64  `json:"not_after_unix_ms"`
	Sequence          uint64 `json:"sequence"`
}

type BridgeCommandReceipt struct {
	Request                 CommandRequest              `json:"request"`
	Acknowledgment          BridgeCommandAcknowledgment `json:"acknowledgment"`
	RequestStartedUnixNS    int64                       `json:"request_started_unix_ns"`
	ResponseCompletedUnixNS int64                       `json:"response_completed_unix_ns"`
	RoundTripElapsedNS      int64                       `json:"round_trip_elapsed_ns"`
}

// CommandReceiptProvider is optional. Existing adapters need not implement it.
// Controller ownership serializes Command and this diagnostic accessor.
type CommandReceiptProvider interface {
	LastCommandReceipt() (BridgeCommandReceipt, bool)
}

func acknowledgmentMatches(a BridgeCommandAcknowledgment, r CommandRequest) bool {
	return a.Accepted && a.Sequence == r.Sequence && a.NotAfterUnixMS == r.NotAfterUnixMS &&
		a.SourceTimestampNS > 0 && a.SourceTimestampNS/int64(time.Millisecond) <= r.NotAfterUnixMS-450
}

func copyReceipt(r *BridgeCommandReceipt) *BridgeCommandReceipt {
	if r == nil {
		return nil
	}
	v := *r
	return &v
}

// CommandDiagnostic is an unsigned observation of one attempt. A failure may
// follow publication; it is never relabeled as proof that nothing was sent.
type CommandDiagnostic struct {
	Request      CommandRequest        `json:"request"`
	Outcome      string                `json:"outcome"`
	ReceiptState string                `json:"receipt_state"`
	Receipt      *BridgeCommandReceipt `json:"bridge_publication_receipt,omitempty"`
	Failure      *BridgeResponseError  `json:"http_refusal,omitempty"`
	Selection    *SelectionDiagnostic  `json:"selection,omitempty"`
}

func copyCommandDiagnostic(in *CommandDiagnostic) *CommandDiagnostic {
	if in == nil {
		return nil
	}
	v := *in
	v.Receipt = copyReceipt(in.Receipt)
	v.Selection = copySelection(in.Selection)
	if in.Failure != nil {
		f := *in.Failure
		v.Failure = &f
	}
	return &v
}

func (c *Controller) recordCommandDiagnostic(r CommandRequest, err error) {
	d := &CommandDiagnostic{Request: r, Outcome: "adapter_call_succeeded", ReceiptState: "unavailable"}
	d.Selection = c.selectionForCommand(r)
	if err != nil {
		d.Outcome = "failed_or_unknown"
		var refusal *BridgeResponseError
		if errors.As(err, &refusal) {
			copy := *refusal
			if !knownBridgeRefusal(copy.Reason) {
				copy.Reason = ""
			}
			d.Failure = &copy
		}
	} else if provider, ok := c.adapter.(CommandReceiptProvider); ok {
		if receipt, available := provider.LastCommandReceipt(); available {
			d.ReceiptState = "mismatched"
			if receipt.Request == r && acknowledgmentMatches(receipt.Acknowledgment, r) &&
				receipt.RequestStartedUnixNS > 0 && receipt.ResponseCompletedUnixNS > 0 && receipt.RoundTripElapsedNS >= 0 {
				d.ReceiptState = "matched"
				d.Outcome = "bridge_publication_acknowledged"
				d.Receipt = copyReceipt(&receipt)
			}
		}
	}
	c.commandDiagnostic = d
	if err == nil {
		// An adapter without a matched receipt must not inherit an earlier ACK.
		c.lastAcknowledgedReceipt = copyReceipt(d.Receipt)
	}
}

// CommandObservation returns only the last attempt, copied for the single
// controller owner. Callers deduplicate by command sequence and join to a sensor
// observation only when its capture sequence exactly matches the request.
func (c *Controller) CommandObservation() (*CommandDiagnostic, bool) {
	return copyCommandDiagnostic(c.commandDiagnostic), c.commandDiagnostic != nil
}
