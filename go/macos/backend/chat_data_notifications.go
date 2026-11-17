package backend

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

var notificationsTool = dataChatTool{Tool: chat.Tool{Name: "device_data_notifications", Description: "Read retained immediate campaign notification intents, detection summaries, current notification errors, and optionally current Cloud permission. Notification intent, permission, provider acceptance and phone receipt are different facts. This tool cannot prove delivery to a recipient. It does not enable notifications. Episode-committed notifications use a separate Cloud path. Read-only.", Parameters: json.RawMessage(`{"type":"object","properties":{"campaign":{"type":"string"},"start":{"type":"string"},"end":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":100},"check_cloud_permission":{"type":"boolean"}},"additionalProperties":false}`)}, run: readChatNotifications}

func readChatNotifications(ctx context.Context, t *deviceDataTools, raw json.RawMessage) (any, error) {
	var a struct {
		Campaign string `json:"campaign"`
		Start    string `json:"start"`
		End      string `json:"end"`
		Limit    int    `json:"limit"`
		Cloud    bool   `json:"check_cloud_permission"`
	}
	if err := decodeDataArgs(raw, &a); err != nil {
		return nil, err
	}
	limit, err := dataLimit(a.Limit)
	if err != nil {
		return nil, err
	}
	w, err := parseDataWindow(a.Start, a.End)
	if err != nil {
		return nil, err
	}
	app := ""
	if a.Campaign != "" {
		app = data.CampaignAppID(a.Campaign)
	}
	r, err := t.conn.DataService.Events(ctx, &pb.DataEventsRequest{AppId: app, Replay: true, NotificationsOnly: true})
	if err != nil {
		return nil, err
	}
	if !r.GetNotifications() {
		return nil, errors.New("agent does not support the separate notification journal; update the agent, do not interpret raw events as notifications")
	}
	if len(r.GetEventsJson()) > 4<<20 {
		return nil, errors.New("notification journal exceeds limit")
	}
	var all []data.CampaignNotification
	if err = json.Unmarshal(r.GetEventsJson(), &all); err != nil {
		return nil, err
	}
	if len(all) > 512 {
		return nil, errors.New("notification journal exceeds entry limit")
	}
	rows := []data.CampaignNotification{}
	unknown := 0
	for _, n := range all {
		if a.Campaign != "" && n.Campaign != a.Campaign {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, n.OccurredAt)
		if err != nil {
			unknown++
			continue
		}
		if w.includes(at) {
			rows = append(rows, n)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, rows[i].OccurredAt)
		b, _ := time.Parse(time.RFC3339Nano, rows[j].OccurredAt)
		if a.Equal(b) {
			return rows[i].Sequence > rows[j].Sequence
		}
		return a.After(b)
	})
	matched := len(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := map[string]any{"notifications": rows, "window": w.evidence(), "matching_retained_notifications": matched, "truncated": matched > limit, "invalid_timestamp_count": unknown, "journal_gap": r.GetGap(), "journal_cursor": r.GetCursor(), "history_complete": false, "recipient_delivery": "unknown", "provider_acceptance": "unknown", "coverage": "Immediate notification intents retained on the device, at most 512. Intent is persisted before delivery attempts. Missing intent does not prove no notification; episode_committed is a separate Cloud path. Current permission does not prove permission at event time. Device push registration and OS permissions are separate."}
	unavailable := map[string]string{}
	if a.Campaign != "" {
		evidence, err := t.campaignEvidence(ctx, a.Campaign)
		if err != nil {
			unavailable["campaign"] = err.Error()
		} else {
			out["campaign"] = evidence
		}
	}
	if a.Cloud {
		if t.conn.ProvisioningService == nil {
			unavailable["cloud_permission"] = "device enrollment service unavailable"
		} else {
			grants, err := commands.DesktopCampaignNotifications(ctx, t.conn)
			if err != nil {
				unavailable["cloud_permission"] = err.Error()
			} else {
				filtered := []commands.DesktopCampaignNotification{}
				for _, g := range grants {
					if a.Campaign == "" || a.Campaign == g.Name {
						filtered = append(filtered, g)
					}
				}
				out["current_cloud_permissions"] = filtered
			}
		}
	}
	out["unavailable"] = unavailable
	return out, nil
}
