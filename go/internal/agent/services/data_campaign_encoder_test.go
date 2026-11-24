package services

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// encoderCampaign is a campaign that records one camera selector, with the
// given lines in its capture block. It has no buffer, so deploying it starts
// no camera.
func encoderCampaign(camera string, capture ...string) []byte {
	yaml := "version: 1\nname: encoder-notice\nsources:\n  - camera: \"" + camera + "\"\n"
	if len(capture) > 0 {
		yaml += "    capture:\n"
		for _, line := range capture {
			yaml += "      " + line + "\n"
		}
	}
	return []byte(yaml + "capture:\n  buffer: 0s\n  drain: 0s\n  after_trigger: 30ms\n  triggers:\n    - event: emergency_stop\nupload:\n  when: manual\nexport:\n  annotation: cvat\n")
}

// deployedOn deploys a campaign to a data service whose cameras are the
// harness's, and returns the service and the deploy's answer.
func deployedOn(t *testing.T, h *csHarness, campaign []byte) (*DataService, *agentpbv2.DataCampaign) {
	t.Helper()
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.SetVideoService(h.s)
	deployed, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: campaign})
	if err != nil {
		t.Fatal(err)
	}
	return service, deployed
}

const c920EncodedInSoftwareWarning = "camera v4l2:/dev/video4: its video is encoded in software (x264enc) on this device while the campaign records or buffers; " +
	"store: frames keeps the camera's own frames without encoding"

// A campaign that stores the video of a webcam on a device without a hardware
// encoder is told at deploy what that video costs and what stores the camera
// without it.
func TestCampaignDeploy_ACameraWhoseStoredVideoIsEncodedInSoftwareIsWarnedOf(t *testing.T) {
	for name, capture := range map[string][]string{
		"store absent": nil,
		"store video":  {"store: video"},
		"store both":   {"store: both"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, csH264Cam(), csC920())
			hcCanDecode(h.s, "avdec_h264", "jpegdec", "x264enc")
			_, deployed := deployedOn(t, h, encoderCampaign("/dev/video4", capture...))
			if got := deployed.GetWarnings(); !slices.Equal(got, []string{c920EncodedInSoftwareWarning}) {
				t.Errorf("warnings = %q\nwant       %q", got, []string{c920EncodedInSoftwareWarning})
			}
			if !strings.Contains(string(deployed.GetPlanJson()), "encoded in software (x264enc)") {
				t.Errorf("the plan the deploy answers with does not hold the warning: %s", deployed.GetPlanJson())
			}
		})
	}
}

func TestCampaignDeploy_NothingIsSaidOfAnEncoderWhereNoSoftwareEncoderMakesStoredVideo(t *testing.T) {
	software := []string{"avdec_h264", "jpegdec", "x264enc"}
	for name, tc := range map[string]struct {
		elements []string
		campaign []byte
	}{
		"store frames":                             {software, encoderCampaign("/dev/video4", "store: frames")},
		"snapshot stills":                          {software, encoderCampaign("/dev/video4", "mode: snapshot", "interval: 1s")},
		"a camera that produces H.264":             {software, encoderCampaign("/dev/video2")},
		"a device with a hardware encoder":         {[]string{"jpegdec", "v4l2h264enc", "x264enc"}, encoderCampaign("/dev/video4")},
		"an encoder that may be hardware":          {[]string{"jpegdec", "vtenc_h264"}, encoderCampaign("/dev/video4", "store: both")},
		"a device that cannot derive H.264":        {[]string{"jpegdec"}, encoderCampaign("/dev/video4")},
		"a camera that is not there at the deploy": {software, encoderCampaign("/dev/video9")},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, csH264Cam(), csC920())
			hcCanDecode(h.s, tc.elements...)
			_, deployed := deployedOn(t, h, tc.campaign)
			if got := deployed.GetWarnings(); len(got) != 0 {
				t.Errorf("warnings = %q, want none", got)
			}
		})
	}
}

// Every camera a selector takes in is looked at on its own, and the warning
// is the deploy's: it is a fact of the cameras plugged in at that moment, so
// the stored plan does not keep it.
func TestCampaignDeploy_TheSoftwareEncoderWarningNamesOnlyItsCameraAndIsNotStoredWithThePlan(t *testing.T) {
	h := newCSHarness(t, csH264Cam(), csC920())
	hcCanDecode(h.s, "avdec_h264", "jpegdec", "x264enc")
	service, deployed := deployedOn(t, h, encoderCampaign("*"))
	if got := deployed.GetWarnings(); !slices.Equal(got, []string{c920EncodedInSoftwareWarning}) {
		t.Errorf("warnings = %q\nwant       %q", got, []string{c920EncodedInSoftwareWarning})
	}
	stored, err := service.CampaignInspect(context.Background(), &agentpbv2.DataCampaignInspectRequest{Name: "encoder-notice"})
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.GetWarnings(); len(got) != 0 {
		t.Errorf("the stored plan holds %q", got)
	}
}
