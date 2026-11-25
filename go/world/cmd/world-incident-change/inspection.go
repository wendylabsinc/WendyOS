package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/incidentchange"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"time"
)

func (r *runtime) prepareInspection(ctx context.Context, digest string) (any, error) {
	if r.estimator != nil || digest == "" || r.observedResult == nil || r.publishedResult == nil || r.p.last == nil || r.p.pending != nil || r.observedResult.InputDigest != digest || r.publishedResult.InputDigest != digest || r.p.last.component.Tombstone || r.p.last.component.Key.Type != incidentchange.Type {
		return nil, errors.New("select an unchanged currently published rising incident")
	}
	if err := r.refreshTrust(); err != nil {
		return nil, err
	}
	snapshot, err := r.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	current, err := r.view.Step(snapshot.Components, time.Now())
	if err != nil {
		return nil, err
	}
	r.observedResult = &current
	if current.State != "supported_rise" || current.InputDigest != digest {
		return nil, errors.New("current replica no longer supports the selected rise")
	}
	now := time.Now()
	c, err := incidentchange.InspectionSelection(r.cfg.Public, r.cfg.Reader, r.p.last.component.Version, *r.publishedResult, r.publishedRecords, now, r.verifyMembers)
	if err != nil {
		return nil, err
	}
	p, _, _, err := missioncontrol.DecodeRisingSelection(c)
	if err != nil || p.JoinedSHA256 != gasreplica.ComponentHash(r.p.last.component) {
		return nil, errors.New("selected rising publication changed")
	}
	trigger, err := tasking.TriggerFromComponent(c)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(trigger)
	if err != nil {
		return nil, err
	}
	if len(raw) > 64<<10 {
		return nil, errors.New("retained trigger exceeds64KiB")
	}
	target, err := missioncontrol.RisingTriggerTarget(c)
	if err != nil {
		return nil, err
	}
	if !time.Now().Before(p.ValidUntil) {
		return nil, errors.New("selected originals expired during preparation")
	}
	hash := sha256.Sum256(r.p.last.body)
	return map[string]any{"version": 1, "trigger_json": raw, "trigger_sha256": trigger.SHA256, "target": target, "envelope_sha256": hex.EncodeToString(hash[:]), "result_schema": missioncontrol.RisingInspectionType, "sample_valid_until": p.ValidUntil, "physical_valid_until": p.ValidUntil}, nil
}
