package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/node"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"os"
	"path/filepath"
)

// Read an authenticated local replica view, retaining its last-known semantics.
// The node checks relay status signatures. This CLI does not invent completion
// while disconnected, and verifies every returned media byte against its digest.
func inspectionResult(ctx context.Context, credentials, evidenceCredentials, id, origin, out string) (any, error) {
	var cfg client.Config
	if e := readJSON(credentials, &cfg, 64<<10, true); e != nil {
		return nil, e
	}
	api, e := client.New(cfg)
	if e != nil {
		return nil, e
	}
	var record tasking.Record
	if origin == "" {
		record, e = api.TaskStatus(ctx, id)
	} else {
		var reply node.TaskRelayResponse
		e = api.Do(ctx, "read", "GET", "/v1/task-relay", nil, &reply)
		found := false
		if e == nil {
			for _, entry := range reply.Entries {
				if entry.Status != nil && entry.Status.Audience == origin && entry.Status.TaskID == id {
					if authority.DecodeStrictLimit(entry.Status.Record, &record, authority.MaxTaskStatusRecordBytes) != nil {
						return nil, errors.New("invalid retained status")
					}
					found = true
					break
				}
			}
			if !found {
				e = errors.New("no retained status from configured origin")
			}
		}
	}
	if e != nil {
		return nil, e
	}
	if record.Spec.ID != id || record.Spec.Assignee != assignee || record.Spec.Trigger == nil || record.Spec.Trigger.Validate(cfg.Grant.Grant.Site, record.Spec.Frame) != nil {
		return nil, errors.New("inspection result scope differs")
	}
	verified := []string{}
	mediaError := ""
	if record.Result != nil && evidenceCredentials != "" {
		var ec client.Config
		if e = readJSON(evidenceCredentials, &ec, 64<<10, true); e != nil {
			return nil, e
		}
		if ec.Grant.Grant.Site != record.Spec.Site || ec.Grant.Grant.Tenant != cfg.Grant.Grant.Tenant {
			return nil, errors.New("evidence scope differs")
		}
		ev, e := client.New(ec)
		if e != nil {
			return nil, e
		}
		if out != "" {
			if !filepath.IsAbs(out) {
				return nil, errors.New("absolute private output required")
			}
			if e = os.MkdirAll(out, 0700); e != nil {
				return nil, e
			}
		}
		for _, s := range record.Result.Samples {
			if s.Evidence == nil {
				continue
			}
			ref := s.Evidence
			raw, meta, e := ev.DownloadEvidence(ctx, ref.SHA256, ref.Size)
			if e != nil {
				mediaError = e.Error()
				break
			}
			if hash(raw) != ref.SHA256 || meta.ContentType != ref.ContentType || int64(len(raw)) != ref.Size {
				return nil, errors.New("inspection evidence digest/metadata differs")
			}
			if out != "" {
				path := filepath.Join(out, ref.SHA256)
				if prior, e := os.ReadFile(path); e == nil {
					if hash(prior) != ref.SHA256 {
						return nil, errors.New("cached evidence changed")
					}
				} else if os.IsNotExist(e) {
					if e = os.WriteFile(path, raw, 0600); e != nil {
						return nil, e
					}
				} else {
					return nil, e
				}
			}
			verified = append(verified, ref.SHA256)
		}
	}
	body, _ := json.Marshal(record)
	return map[string]any{"record": record, "record_sha256": hash(body), "last_known": origin != "", "verified_media": verified, "media_error": mediaError, "knowledge": fmt.Sprintf("Authenticated %s replica view; remote state can be newer", cfg.Audience)}, nil
}
