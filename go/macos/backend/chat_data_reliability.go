package backend

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

var healthTool = dataChatTool{Tool: chat.Tool{Name: "device_data_health", Description: "Inspect current camera/sensor availability and in-progress recordings. With campaign, include current capture, inference, upload and retention policy. With episode, include historical losses, clock evidence, interruption, file sizes and actual upload workflow. Optionally verify episode files. Separates current health from recorded coverage. No deletion or upload. Read-only.", Parameters: json.RawMessage(`{"type":"object","properties":{"campaign":{"type":"string"},"episode":{"type":"string"},"verify":{"type":"boolean","description":"Verify stored episode files; may be slow, requires episode"}},"additionalProperties":false}`)}, run: readChatHealth}

func readChatHealth(ctx context.Context, t *deviceDataTools, raw json.RawMessage) (any, error) {
	var a struct {
		Campaign string `json:"campaign"`
		Episode  string `json:"episode"`
		Verify   bool   `json:"verify"`
	}
	if err := decodeDataArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Verify && a.Episode == "" {
		return nil, errors.New("verify requires an episode")
	}
	out := map[string]any{"historical_armed_coverage": "unknown; current campaign state is not an arming history", "deletion_time": "unknown; quota policy is not an expiration schedule"}
	unavailable := map[string]string{}
	sources, err := t.conn.DataService.Sources(ctx, &pb.DataSourcesRequest{})
	if err != nil {
		unavailable["current_sources"] = err.Error()
	} else {
		out["current_sources"] = sources.GetSources()
	}
	status, err := t.conn.DataService.Status(ctx, &pb.DataStatusRequest{})
	if err != nil {
		unavailable["current_recordings"] = err.Error()
	} else {
		out["current_recordings"] = status
	}
	if a.Campaign != "" {
		campaign, err := t.campaignEvidence(ctx, a.Campaign)
		if err != nil {
			unavailable["campaign"] = err.Error()
		} else {
			out["campaign"] = campaign
		}
	}
	if a.Episode != "" {
		m, verification, err := t.manifest(ctx, a.Episode, a.Verify)
		if err != nil {
			unavailable["episode"] = err.Error()
		} else {
			var bytes int64
			validSizes := true
			for _, f := range m.Files {
				if f.Size < 0 || bytes > int64(^uint64(0)>>1)-f.Size {
					validSizes = false
					break
				}
				bytes += f.Size
			}
			var size any
			if validSizes {
				size = bytes
			}
			out["episode"] = map[string]any{"id": m.ID, "state": m.State, "trigger": chatTriggerEvidence(m.Trigger), "interruption": m.Interruption, "sources": m.Sources, "pre_roll_lost": m.PreRollLost, "pre_roll_accounting": m.PreRollAccounting, "model_io": m.ModelIO, "recovery_actions": m.RecoveryActions, "playable_notes": m.PlayableNotes, "system_clock_status": m.SystemClockStatus, "utc_observations": m.UTCObservations, "boot_id": m.BootID, "started_unix_nanos": m.StartedUnixNanos, "request_boottime_nanos": m.RequestBootNanos, "upload": m.Upload, "local_manifest_file_bytes": size, "file_count": len(m.Files), "verification_requested": a.Verify, "verification_errors": verification, "storage_location": "Episode on original chat device. Upload state/destination are the device's last report, not a fresh Cloud inventory check."}
		}
	}
	out["unavailable"] = unavailable
	out["interpretation"] = "Source health is current. Source counts, drops and discontinuities describe only the selected episode; absent drop counts remain unknown. Capture may intentionally sample. Capture stop and sealing differ. Retention local_quota may be advisory; heed campaign warnings. Do not invent a deletion deadline, historical uptime, Cloud availability, or causal diagnosis."
	return out, nil
}
func (t *deviceDataTools) campaignEvidence(ctx context.Context, name string) (map[string]any, error) {
	r, err := t.conn.DataService.CampaignInspect(ctx, &pb.DataCampaignInspectRequest{Name: name})
	if err != nil {
		return nil, err
	}
	if r.GetName() != name {
		return nil, errors.New("agent returned a different campaign")
	}
	if len(r.GetPlanJson()) > 1<<20 {
		return nil, errors.New("campaign plan exceeds limit")
	}
	var plan data.Campaign
	if err = json.Unmarshal(r.GetPlanJson(), &plan); err != nil {
		return nil, err
	}
	var notificationPolicy any
	if plan.Notify != nil {
		notificationPolicy = map[string]any{"on": plan.Notify.On, "event": plan.Notify.Event, "webhook_configured": plan.Notify.Webhook != ""}
	}
	// Never expose notify webhook credentials or model environment variables.
	return map[string]any{"name": name, "state": r.GetState(), "revision": r.GetRevision(), "deployed_unix_nanos": r.GetDeployedUnixNanos(), "sources": plan.Sources, "capture": plan.Capture, "inference_status": plan.InferenceStatus, "inference_policy": plan.Inference, "upload_policy": plan.Upload, "retention_policy": plan.Retention, "notifications_configured": plan.Notify != nil, "notification_policy": notificationPolicy, "warnings": r.GetWarnings()}, nil
}
