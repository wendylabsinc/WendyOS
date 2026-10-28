package localmesh

import "fmt"

func diagnosticText(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func boundedCarrierCloseReason(stage string, cause error) string {
	reason := "owner requested closure"
	if cause != nil {
		reason = diagnosticText(cause.Error(), 256)
	}
	return fmt.Sprintf("%s: %s", diagnosticText(stage, 48), reason)
}
