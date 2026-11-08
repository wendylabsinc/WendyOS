package chat

import (
	"encoding/json"
	"sync/atomic"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// The Wendy MCP server's watch notifications (design §6.6).
const (
	watchEventMethod   = "notifications/wendy/watch_event"
	watchStatusMethod  = "notifications/wendy/watch_status"
	watchStoppedReason = "stopped" // the reason watch_stop gives
	watchInboxSize     = 64
)

// WatchClass is one detected class and its score.
type WatchClass struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
}

// WatchNotice is one watch notification: an event (Kind "entered" or "left")
// or a status (State). Missed counts notices this inbox dropped before this
// one; MissedByServer counts notifications the server could not send.
type WatchNotice struct {
	Method         string       `json:"-"`
	WatchID        string       `json:"watch_id"`
	Label          string       `json:"label"`
	Sequence       uint64       `json:"sequence,omitempty"`
	Kind           string       `json:"kind,omitempty"`
	Classes        []WatchClass `json:"classes,omitempty"`
	OccurredAt     string       `json:"occurred_at,omitempty"`
	State          string       `json:"state,omitempty"`
	Reason         string       `json:"reason,omitempty"`
	Camera         string       `json:"camera,omitempty"`
	Watching       []string     `json:"watching,omitempty"`
	MissedByServer int          `json:"missed_notifications,omitempty"`
	Missed         int          `json:"-"`
}

// watchInbox receives watch notifications from the MCP client (design §7.1).
type watchInbox struct {
	items   chan WatchNotice
	dropped atomic.Int64
}

func newWatchInbox() *watchInbox { return &watchInbox{items: make(chan WatchNotice, watchInboxSize)} }

// handle runs on mcp-go's stdio reader goroutine, which also delivers tool
// results, so it never blocks: a full inbox drops the notice and counts it.
func (b *watchInbox) handle(n mcpgo.JSONRPCNotification) {
	if n.Method != watchEventMethod && n.Method != watchStatusMethod {
		return
	}
	raw, err := json.Marshal(n.Params.AdditionalFields)
	if err != nil {
		return
	}
	var notice WatchNotice
	if json.Unmarshal(raw, &notice) != nil || notice.WatchID == "" {
		return
	}
	notice.Method = n.Method
	notice.Missed = int(b.dropped.Swap(0))
	select {
	case b.items <- notice:
	default:
		b.dropped.Add(int64(notice.Missed) + 1)
	}
}
