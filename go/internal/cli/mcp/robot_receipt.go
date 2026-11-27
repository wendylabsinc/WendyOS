package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Match chat's robot control transport budget, independently of display limits.
// Dense hazard snapshots can exceed 256 KiB. Limiting a streaming decoder to
// that size silently truncated valid receipts into "unexpected EOF" errors.
const maxRobotReceiptBytes = 1 << 20

func decodeRobotReceipt(reader io.Reader) (map[string]any, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxRobotReceiptBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading robot receipt: %w", err)
	}
	if len(data) > maxRobotReceiptBytes {
		return nil, errors.New("robot receipt exceeds the 1 MiB transport limit")
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decoding robot receipt: %w", err)
	}
	if out == nil {
		return nil, errors.New("robot response is missing its command receipt")
	}
	return out, nil
}
