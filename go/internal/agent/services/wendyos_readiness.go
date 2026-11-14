package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const wendyOSReadinessLimit = 64 * 1024

// updaterRootfsRefusal asks the exact selected engine whether its rootfs
// strategy is ready. Current Orin engines use boot-chain A/B, which does not
// require the legacy RootfsRedundancyLevel setting. Call only for the typed,
// validated volatile-zero incompatibility; all other preparation runs normally.
// It returns "" when the engine is ready. Older engines and unavailable or
// ambiguous verdicts retain the legacy refusal, and the returned reason says
// which of those it was so the caller can report more than "reflash".
func updaterRootfsRefusal(ctx context.Context, binary string) string {
	probe, cancel := context.WithTimeout(ctx, wendyOSDetectTimeout)
	defer cancel()
	cmd := exec.CommandContext(probe, binary, "check", "--json")
	cmd.Env = envWithPath("/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	// Bound both captured output and a descendant retaining the stdout pipe.
	cmd.WaitDelay = 250 * time.Millisecond
	output := &readinessOutput{}
	cmd.Stdout = output
	runErr := cmd.Run()
	if probe.Err() != nil {
		return "no verdict: " + probe.Err().Error()
	}
	refusal := rootfsReadinessRefusal(output.buffer.Bytes())
	// `check` exits 5 with a well-formed verdict naming the failing checks;
	// any other failed run has nothing more specific to report than its error.
	if runErr != nil && (refusal == "" || refusal == malformedReadiness) {
		return "no verdict: " + runErr.Error()
	}
	return refusal
}

type readinessOutput struct{ buffer bytes.Buffer }

func (b *readinessOutput) Write(p []byte) (int, error) {
	if len(p) > wendyOSReadinessLimit-b.buffer.Len() {
		return 0, errors.New("updater readiness output exceeds limit")
	}
	return b.buffer.Write(p)
}

const (
	rootfsReadinessCheck = "rootfs A/B redundancy"
	malformedReadiness   = "malformed verdict"
	// Check names come from the updater's output and end up in a user-facing
	// error; keep what is echoed short.
	readinessReasonLimit = 256
)

// rootfsReadinessRefusal returns "" only for a verdict in which every check,
// including the rootfs one, passed; otherwise why the verdict does not delegate.
func rootfsReadinessRefusal(raw []byte) string {
	if len(raw) > wendyOSReadinessLimit {
		return malformedReadiness
	}
	// encoding/json otherwise accepts duplicate keys with last-value-wins.
	d := json.NewDecoder(bytes.NewReader(raw))
	if !readinessJSONValue(d, 0) {
		return malformedReadiness
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return malformedReadiness
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil {
		return malformedReadiness
	}
	var checks []map[string]json.RawMessage
	if json.Unmarshal(result["checks"], &checks) != nil {
		return malformedReadiness
	}
	found := false
	names := map[string]bool{}
	var failed []string
	for _, check := range checks {
		var name string
		if json.Unmarshal(check["name"], &name) != nil || name == "" || names[name] {
			return malformedReadiness
		}
		names[name] = true
		switch string(check["ok"]) {
		case "true":
		case "false":
			failed = append(failed, strconv.Quote(name))
		default:
			return malformedReadiness
		}
		if name == rootfsReadinessCheck {
			found = true
		}
	}
	if len(failed) > 0 {
		reason := "failed checks: " + strings.Join(failed, ", ")
		if len(reason) > readinessReasonLimit {
			reason = reason[:readinessReasonLimit] + "…"
		}
		return reason
	}
	if string(result["ok"]) != "true" {
		return malformedReadiness
	}
	if !found {
		return "verdict did not report " + strconv.Quote(rootfsReadinessCheck)
	}
	return ""
}

// Accept additive fields while refusing duplicate keys at every object level.
func readinessJSONValue(d *json.Decoder, depth int) bool {
	if depth > 32 {
		return false
	}
	token, err := d.Token()
	if err != nil {
		return false
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return true
	}
	switch delimiter {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			token, err := d.Token()
			key, ok := token.(string)
			if err != nil || !ok || keys[key] {
				return false
			}
			keys[key] = true
			if !readinessJSONValue(d, depth+1) {
				return false
			}
		}
		token, err = d.Token()
		return err == nil && token == json.Delim('}')
	case '[':
		for d.More() {
			if !readinessJSONValue(d, depth+1) {
				return false
			}
		}
		token, err = d.Token()
		return err == nil && token == json.Delim(']')
	default:
		return false
	}
}
