package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"gopkg.in/yaml.v3"
	"strings"
)

type CampaignSettings struct {
	Name, Revision, Before, After, Upload, Cooldown string
	Inference                                       bool
	Threshold                                       float64
}

func decodeCampaignSettings(raw []byte) (data.Campaign, error) {
	var plan data.Campaign
	decoder := json.NewDecoder(bytes.NewReader(raw))
	// Refuse newer fields rather than silently dropping them on a save.
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&plan)
	return plan, err
}
func campaignSettings(value *pb.DataCampaign) (CampaignSettings, error) {
	plan, err := decodeCampaignSettings(value.GetPlanJson())
	if err != nil {
		return CampaignSettings{}, fmt.Errorf("cannot edit this campaign plan: %w", err)
	}
	result := CampaignSettings{Name: value.GetName(), Revision: value.GetRevision(), Before: plan.Capture.Buffer, After: plan.Capture.AfterTrigger, Upload: plan.Upload.When}
	if plan.Inference != nil {
		result.Inference = true
		result.Threshold = plan.Inference.Threshold
		result.Cooldown = plan.Inference.Cooldown
	}
	return result, nil
}
func (b *Backend) CampaignSettings(session uint64, name string) (CampaignSettings, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 || strings.TrimSpace(name) == "" {
		return CampaignSettings{}, errors.New("select a campaign first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return CampaignSettings{}, err
	}
	value, err := conn.DataService.CampaignInspect(ctx, &pb.DataCampaignInspectRequest{Name: name})
	if err != nil {
		return CampaignSettings{}, err
	}
	return campaignSettings(value)
}
func patchCampaignSettings(current *pb.DataCampaign, in CampaignSettings) ([]byte, error) {
	if in.Revision == "" || current.GetRevision() != in.Revision || current.GetName() != in.Name {
		return nil, errors.New("this campaign changed; reopen settings before saving")
	}
	plan, err := decodeCampaignSettings(current.GetPlanJson())
	if err != nil {
		return nil, err
	}
	if plan.Name != in.Name {
		return nil, errors.New("campaign name does not match its plan")
	}
	plan.Capture.Buffer = in.Before
	plan.Capture.AfterTrigger = in.After
	plan.Upload.When = in.Upload
	if plan.Inference != nil {
		plan.Inference.Threshold = in.Threshold
		plan.Inference.Cooldown = in.Cooldown
	}
	raw, err := yaml.Marshal(plan)
	if err != nil {
		return nil, err
	}
	if _, err = data.ParseCampaign(raw); err != nil {
		return nil, err
	}
	return raw, nil
}
func (b *Backend) UpdateCampaignSettings(session uint64, in CampaignSettings) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 || strings.TrimSpace(in.Name) == "" {
		return errors.New("select a campaign first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	current, err := conn.DataService.CampaignInspect(ctx, &pb.DataCampaignInspectRequest{Name: in.Name})
	if err != nil {
		return err
	}
	raw, err := patchCampaignSettings(current, in)
	if err != nil {
		return err
	}
	_, err = conn.DataService.CampaignDeploy(ctx, &pb.DataCampaignDeployRequest{CampaignYaml: raw})
	return err
}
