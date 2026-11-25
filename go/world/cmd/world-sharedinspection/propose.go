package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

// These bytes come from the selected ordinary reader. They remain historical
// observation context, never an instruction or a claim of current freshness.
type missionInput struct {
	Version            int        `json:"version"`
	TriggerJSON        []byte     `json:"trigger_json"`
	TriggerSHA256      string     `json:"trigger_sha256"`
	Target             [3]float64 `json:"target"`
	EnvelopeSHA256     string     `json:"envelope_sha256"`
	ResultSchema       string     `json:"result_schema"`
	SampleValidUntil   time.Time  `json:"sample_valid_until"`
	PhysicalValidUntil time.Time  `json:"physical_valid_until"`
}

type handoff struct {
	Version                int                                   `json:"version"`
	ProposalID             string                                `json:"proposal_id"`
	MissionInput           missionInput                          `json:"mission_input"`
	OriginalEnvelopeSHA256 string                                `json:"original_envelope_sha256"`
	NavigationTarget       *[3]float64                           `json:"navigation_target,omitempty"`
	InspectionDestination  *missioncontrol.InspectionDestination `json:"inspection_destination,omitempty"`
	RouteLengthM           *float64                              `json:"route_length_m,omitempty"`
	NominalTravelMS        *int64                                `json:"nominal_travel_ms,omitempty"`
	MinimumDurationMS      *int64                                `json:"minimum_duration_ms,omitempty"`
	Navigation             *navigationReview                     `json:"navigation,omitempty"`
}

func proposalRequest(c client.Config, input missionInput, id string, now time.Time) (proposals.Request, error) {
	var zero proposals.Request
	var trigger tasking.TriggerRef
	if input.Version != 1 || authority.DecodeStrictLimit(input.TriggerJSON, &trigger, 512<<10) != nil || trigger.Validate(c.Grant.Grant.Site, trigger.Component.Frame) != nil || input.TriggerSHA256 != trigger.SHA256 || !digest(input.EnvelopeSHA256) || (input.ResultSchema != "wendy.person.track.v1" && input.ResultSchema != "wendy.hazard.v1" && input.ResultSchema != missioncontrol.IncidentInspectionType && input.ResultSchema != missioncontrol.RisingInspectionType) || input.SampleValidUntil.IsZero() || input.PhysicalValidUntil.IsZero() {
		return zero, errors.New("exact reader person or hazard mission input required")
	}
	canonical, e := json.Marshal(trigger)
	if e != nil || !bytes.Equal(canonical, input.TriggerJSON) {
		return zero, errors.New("canonical Go trigger bytes required")
	}
	// The commissioned floor fills the missing person Y only. A hazard keeps its
	// original three-dimensional location; proposal preparation does not project
	// it onto the planar robot's floor or claim the navigation profile accepts it.
	target, e := missioncontrol.NativeTriggerTarget(trigger.Component, .24)
	var result struct {
		ResultSchema string `json:"result_schema"`
	}
	_ = json.Unmarshal(trigger.Component.Payload, &result)
	if trigger.Component.Key.Type == missioncontrol.IncidentInspectionType || trigger.Component.Key.Type == missioncontrol.RisingInspectionType {
		result.ResultSchema = trigger.Component.Key.Type
	}
	g := c.Grant.Grant
	runID := strings.TrimPrefix(trigger.Component.Timing.ClockDomain, "simulation:")
	if e != nil || target != input.Target || result.ResultSchema != input.ResultSchema || trigger.Component.Key.Tenant != g.Tenant || trigger.Component.Key.Site != g.Site || g.Domain != "simulation" || g.Kind != "agent" || !((g.ID == "sharedinspection/"+runID+"/proposer" && c.Audience == audience) || (g.ID == "incidentinspection/"+runID+"/proposer" && c.Audience == "node-2" && (input.ResultSchema == missioncontrol.IncidentInspectionType || input.ResultSchema == missioncontrol.RisingInspectionType))) || !reflect.DeepEqual(g.Scopes, []string{"propose", "read"}) || !proposals.ValidID(id) {
		return zero, errors.New("proposal scope, fixed target or role differs")
	}
	until := now.Add(5 * time.Minute)
	if limit := time.UnixMilli(g.NotAfterUnixMS); limit.Before(until) {
		until = limit
	}
	if !until.After(now) {
		return zero, errors.New("proposer grant expired")
	}
	parameters, _ := json.Marshal(map[string]any{"target": input.Target, "trigger_sha256": input.TriggerSHA256})
	return proposals.Request{Version: 1, ID: id, Tenant: g.Tenant, Site: g.Site, RunID: runID, Assignee: assignee, Type: missioncontrol.InspectType, Parameters: parameters, Summary: "Inspect this fixed observed location.", NotAfterUnixMS: until.UnixMilli()}, nil
}

type proposalClient interface {
	ProposeTask(context.Context, proposals.Request) (proposals.View, error)
}

func submitProposal(ctx context.Context, c proposalClient, request proposals.Request, input missionInput) (handoff, error) {
	view, e := c.ProposeTask(ctx, request)
	if e != nil {
		return handoff{}, e
	}
	body, _ := json.Marshal(request)
	if view.Expired || view.AdapterValidated || !reflect.DeepEqual(view.Record.Proposal, request) || !bytes.Equal(view.Record.Body, body) || view.Record.BodySHA256 != hash(body) {
		return handoff{}, errors.New("proposal acknowledgment differs; outcome unknown")
	}
	return handoff{Version: 1, ProposalID: request.ID, MissionInput: input, OriginalEnvelopeSHA256: input.EnvelopeSHA256}, nil
}

func propose(ctx context.Context, credentials, inputPath, output, id string, navigationDirectory, readTokenPath string) error {
	var c client.Config
	var input missionInput
	if e := readJSON(credentials, &c, 64<<10, true); e != nil {
		return e
	}
	if e := readJSON(inputPath, &input, 768<<10, false); e != nil {
		return e
	}
	request, e := proposalRequest(c, input, id, time.Now())
	if e != nil {
		return e
	}
	var navigation *navigationPlan
	if navigationDirectory != "" || readTokenPath != "" {
		navigation, e = prepareNavigation(ctx, c, input, navigationDirectory, readTokenPath)
		if e != nil {
			return e
		}
		request.Parameters, e = json.Marshal(missioncontrol.InspectParameters{Target: navigation.Target,
			TriggerSHA256: input.TriggerSHA256, InspectionDestination: &navigation.Descriptor})
		if e != nil {
			return e
		}
		request.Summary = "Inspect a reachable point near this fixed observation."
	}
	api, e := client.New(c)
	if e != nil {
		return e
	}
	value, e := submitProposal(ctx, api, request, input)
	if e != nil {
		return e
	}
	if navigation != nil {
		value.NavigationTarget = &navigation.Target
		value.InspectionDestination = &navigation.Descriptor
		value.RouteLengthM = &navigation.LengthM
		value.NominalTravelMS = &navigation.NominalTravelMS
		value.MinimumDurationMS = &navigation.MinimumDurationMS
		value.Navigation = &navigation.Review
	}
	return writeNew(output, value)
}
