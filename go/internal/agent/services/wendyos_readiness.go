package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"time"
)

const wendyOSReadinessLimit = 64 * 1024

// updaterRootfsReady asks the exact selected engine whether its rootfs strategy
// is ready. Current Orin engines use boot-chain A/B, which does not require the
// legacy RootfsRedundancyLevel setting. Call only for the typed, validated
// volatile-zero incompatibility; all other preparation runs normally. Older
// engines and unavailable/ambiguous verdicts retain the legacy refusal.
func updaterRootfsReady(ctx context.Context, binary string) bool {
	probe, cancel := context.WithTimeout(ctx, wendyOSDetectTimeout)
	defer cancel()
	cmd := exec.CommandContext(probe, binary, "check", "--json")
	cmd.Env = envWithPath("/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	// Bound both captured output and a descendant retaining the stdout pipe.
	cmd.WaitDelay = 250 * time.Millisecond
	output := &readinessOutput{}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil || probe.Err() != nil {
		return false
	}
	return validRootfsReadiness(output.buffer.Bytes())
}

type readinessOutput struct{ buffer bytes.Buffer }

func (b *readinessOutput) Write(p []byte) (int, error) {
	if len(p) > wendyOSReadinessLimit-b.buffer.Len() {
		return 0, errors.New("updater readiness output exceeds limit")
	}
	return b.buffer.Write(p)
}

func validRootfsReadiness(raw []byte) bool {
	if len(raw) > wendyOSReadinessLimit {
		return false
	}
	// encoding/json otherwise accepts duplicate keys with last-value-wins.
	d := json.NewDecoder(bytes.NewReader(raw))
	if !readinessJSONValue(d, 0) {
		return false
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return false
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil || string(result["ok"]) != "true" {
		return false
	}
	var checks []map[string]json.RawMessage
	if json.Unmarshal(result["checks"], &checks) != nil {
		return false
	}
	found := false
	names := map[string]bool{}
	for _, check := range checks {
		var name string
		if json.Unmarshal(check["name"], &name) != nil || name == "" || names[name] || string(check["ok"]) != "true" {
			return false
		}
		names[name] = true
		if name == "rootfs A/B redundancy" {
			found = true
		}
	}
	return found
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
