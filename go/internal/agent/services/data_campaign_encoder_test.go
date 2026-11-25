package services

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// encoderPlan writes a campaign from its camera sources, each a selector and
// the lines of its capture block, and from what distinguishes the plan: its
// buffer and whether it runs inference.
type encoderPlan struct {
	sources   [][]string
	buffer    string
	inference bool
	telemetry bool
}

func cameraSource(selector string, capture ...string) []string {
	return append([]string{selector}, capture...)
}

func (p encoderPlan) yaml() []byte {
	yaml := "version: 1\nname: encoder-notice\nsources:\n"
	if p.telemetry {
		yaml += "  - telemetry: true\n"
	}
	for _, source := range p.sources {
		yaml += "  - camera: \"" + source[0] + "\"\n"
		if len(source) > 1 {
			yaml += "    capture:\n"
			for _, line := range source[1:] {
				yaml += "      " + line + "\n"
			}
		}
	}
	event := "emergency_stop"
	if p.inference {
		event = "person_detected"
		yaml += "inference:\n  model: facebook/detr-resnet-50\n  revision: 1d5f47bd3bdd2c4bbfa585418ffe6da5028b4c0b\n  labels: [person]\n" +
			"  threshold: 0.9\n  rate: 1\n  event: person_detected\n  clear_after: 5s\n  cooldown: 30s\n"
	}
	buffer := p.buffer
	if buffer == "" {
		buffer = "0s"
	}
	return []byte(yaml + "capture:\n  buffer: " + buffer + "\n  drain: 0s\n  after_trigger: 30ms\n  triggers:\n    - event: " + event +
		"\nupload:\n  when: manual\nexport:\n  annotation: cvat\n")
}

func recording(sources ...[]string) encoderPlan { return encoderPlan{sources: sources} }

// encoderCameras is a device with a camera that produces H.264 at /dev/video2,
// a webcam captured as JPEG at /dev/video4 and one at /dev/video6 that offers
// only uncompressed video, and these GStreamer elements beside the ones that
// frame H.264.
func encoderCameras(t *testing.T, elements ...string) *csHarness {
	t.Helper()
	uncompressed := csCamera{"video6", "Raw Cam", []uint32{v4l2PixFmtYUYV},
		camera.Identity{Device: csPort("1-2.4"), Index: "0", Name: "Raw Cam"}}
	h := newCSHarness(t, csH264Cam(), csC920(), uncompressed)
	hcCanDecode(h.s, elements...)
	offersJPEG := deviceSupportsMJPEGSize
	deviceSupportsMJPEGSize = func(path string, w, h uint32) bool { return path != "/dev/video6" && offersJPEG(path, w, h) }
	return h
}

var softwareEncoderElements = []string{"avdec_h264", "jpegdec", "x264enc"}

// deployWarningsOf is what the camera adapter says of a plan on the harness's
// device, without deploying it.
func deployWarningsOf(t *testing.T, h *csHarness, plan encoderPlan) []string {
	t.Helper()
	campaign, err := data.ParseCampaign(plan.yaml())
	if err != nil {
		t.Fatal(err)
	}
	return newCameraDataAdapter(h.s).(dataDeployAdvisingAdapter).DeployWarnings(context.Background(), campaign)
}

// deployedOn deploys a plan to a data service whose cameras are the
// harness's, and returns the service and the deploy's answer.
func deployedOn(t *testing.T, h *csHarness, plan encoderPlan) (*DataService, *agentpbv2.DataCampaign) {
	t.Helper()
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.SetVideoService(h.s)
	deployed, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: plan.yaml()})
	if err != nil {
		t.Fatal(err)
	}
	return service, deployed
}

const (
	atTheDefaultPreset    = ", at preset ultrafast or the fastest one a consumer of the camera states,"
	encodedInSoftware     = ": its video is encoded in software (x264enc)" + atTheDefaultPreset + " on this device while "
	c920RecordedWarning   = "camera v4l2:/dev/video4" + encodedInSoftware + "the campaign records or buffers; store: frames keeps the camera's own frames without encoding"
	rawCamRecordedWarning = "camera v4l2:/dev/video6" + encodedInSoftware + "the campaign records or buffers"
	noFramesOfItsOwn      = ": it hands on no frames of its own, so what its source asks to store is recorded as video, encoded in software (x264enc)" + atTheDefaultPreset + " on this device while the campaign records or buffers"
	c920InferenceWarning  = "camera v4l2:/dev/video4" + encodedInSoftware + "the campaign's inference runs"
)

// A campaign that stores the video of a webcam this device encodes in
// software is told at deploy what makes that video and what stores the camera
// without it.
func TestCampaignDeploy_ACameraWhoseStoredVideoIsEncodedInSoftwareIsWarnedOf(t *testing.T) {
	for name, capture := range map[string][]string{
		"store absent": nil,
		"store video":  {"store: video"},
		"store both":   {"store: both"},
	} {
		t.Run(name, func(t *testing.T) {
			h := encoderCameras(t, softwareEncoderElements...)
			_, deployed := deployedOn(t, h, recording(cameraSource("/dev/video4", capture...)))
			if got := deployed.GetWarnings(); !slices.Equal(got, []string{c920RecordedWarning}) {
				t.Errorf("warnings = %q\nwant       %q", got, []string{c920RecordedWarning})
			}
			if !strings.Contains(string(deployed.GetPlanJson()), "encoded in software (x264enc)") {
				t.Errorf("the plan the deploy answers with does not hold the warning: %s", deployed.GetPlanJson())
			}
		})
	}
}

func TestCampaignDeploy_NothingIsSaidOfAnEncoderWhereNoSoftwareEncoderMakesStoredVideo(t *testing.T) {
	for name, tc := range map[string]struct {
		elements []string
		plan     encoderPlan
	}{
		"store frames":                             {softwareEncoderElements, recording(cameraSource("/dev/video4", "store: frames"))},
		"snapshot stills":                          {softwareEncoderElements, recording(cameraSource("/dev/video4", "mode: snapshot", "interval: 1s"))},
		"a camera that produces H.264":             {softwareEncoderElements, recording(cameraSource("/dev/video2"))},
		"a hardware encoder chosen before x264enc": {[]string{"jpegdec", "v4l2h264enc", "x264enc"}, recording(cameraSource("/dev/video4"), cameraSource("/dev/video6"))},
		"an encoder that may be hardware":          {[]string{"jpegdec", "vtenc_h264"}, recording(cameraSource("*", "store: both"))},
		"a device that encodes VP8":                {[]string{"jpegdec", "vp8enc", "webmmux"}, recording(cameraSource("*"))},
		"a camera that is not there at the deploy": {softwareEncoderElements, recording(cameraSource("/dev/video9"))},
	} {
		t.Run(name, func(t *testing.T) {
			h := encoderCameras(t, tc.elements...)
			_, deployed := deployedOn(t, h, tc.plan)
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
	h := encoderCameras(t, softwareEncoderElements...)
	service, deployed := deployedOn(t, h, recording(cameraSource("*")))
	want := []string{c920RecordedWarning, rawCamRecordedWarning}
	if got := deployed.GetWarnings(); !slices.Equal(got, want) {
		t.Errorf("warnings = %q\nwant       %q", got, want)
	}
	stored, err := service.CampaignInspect(context.Background(), &agentpbv2.DataCampaignInspectRequest{Name: "encoder-notice"})
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.GetWarnings(); len(got) != 0 {
		t.Errorf("the stored plan holds %q", got)
	}
}

// A camera listed again is recorded with the last capture block a listing
// gives it, so a later listing without one changes nothing of what a
// recording subscribes to, and the deploy says nothing of video nobody
// stores.
func TestCampaignDeploy_ACameraListedAgainWithoutACaptureBlockIsNotWarnedOfVideoItDoesNotStore(t *testing.T) {
	for name, plan := range map[string]encoderPlan{
		"frames, then the camera again":              recording(cameraSource("/dev/video4", "store: frames"), cameraSource("/dev/video4")),
		"frames for every camera, then one again":    recording(cameraSource("*", "store: frames"), cameraSource("/dev/video4")),
		"snapshots, then every camera without block": recording(cameraSource("/dev/video4", "mode: snapshot", "interval: 1s"), cameraSource("*")),
	} {
		t.Run(name, func(t *testing.T) {
			h := newCSHarness(t, csH264Cam(), csC920())
			hcCanDecode(h.s, softwareEncoderElements...)
			_, deployed := deployedOn(t, h, plan)
			// A selector written twice has a warning of its own.
			for _, warning := range deployed.GetWarnings() {
				if strings.Contains(warning, "encode") {
					t.Errorf("warned %q: the recording subscribes for the camera's own frames only", warning)
				}
			}
			if got := deployWarningsOf(t, h, plan); len(got) != 0 {
				t.Errorf("the camera adapter says %q, want nothing", got)
			}
		})
	}
}

// With a buffer, arming is handed every listing and the last one it keeps a
// ring for decides what the camera is buffered as: the bare listing makes it
// video, whatever the recording then stores.
func TestCampaignDeployWarnings_ABufferedCameraIsWarnedOfByWhatArmingSubscribesTo(t *testing.T) {
	h := newCSHarness(t, csH264Cam(), csC920())
	hcCanDecode(h.s, softwareEncoderElements...)
	// The plan already asks this camera for frames in one listing, so the
	// warning does not advise what it does.
	const c920WithoutAdvice = "camera v4l2:/dev/video4" + encodedInSoftware + "the campaign records or buffers"
	framesThenBare := encoderPlan{buffer: "2s", sources: [][]string{cameraSource("/dev/video4", "store: frames"), cameraSource("/dev/video4")}}
	if got := deployWarningsOf(t, h, framesThenBare); !slices.Equal(got, []string{c920WithoutAdvice}) {
		t.Errorf("buffered as video: warnings = %q\nwant %q", got, []string{c920WithoutAdvice})
	}
	bareThenFrames := encoderPlan{buffer: "2s", sources: [][]string{cameraSource("/dev/video4"), cameraSource("/dev/video4", "store: frames")}}
	if got := deployWarningsOf(t, h, bareThenFrames); len(got) != 0 {
		t.Errorf("buffered and recorded as frames: warnings = %q, want none", got)
	}
	// Arming keeps no ring for a snapshot listing, so the one before it stands.
	bareThenSnapshot := encoderPlan{buffer: "2s", sources: [][]string{cameraSource("/dev/video4"), cameraSource("/dev/video4", "mode: snapshot", "interval: 1s")}}
	if got := deployWarningsOf(t, h, bareThenSnapshot); !slices.Equal(got, []string{c920RecordedWarning}) {
		t.Errorf("buffered as video before a snapshot listing: warnings = %q\nwant %q", got, []string{c920RecordedWarning})
	}
}

// A camera no native source captures is encoded by the GStreamer pipeline
// with the same chosen encoder. It hands on no frames of its own, so the
// warning gives the fact and not the advice.
func TestCampaignDeployWarnings_ACameraThePipelineEncodesInSoftwareIsWarnedOfWithoutTheFramesAdvice(t *testing.T) {
	const c920WithoutAdvice = "camera v4l2:/dev/video4" + encodedInSoftware + "the campaign records or buffers"
	for name, tc := range map[string]struct {
		elements []string
		prepare  func(h *csHarness)
		camera   string
		want     string
	}{
		"a webcam that offers only uncompressed video": {softwareEncoderElements, func(*csHarness) {}, "/dev/video6", rawCamRecordedWarning},
		"a webcam whose derived frames failed recently": {softwareEncoderElements, func(h *csHarness) {
			h.s.derivedFailures.remember(h.s.cameraIdentityKey("/dev/video4"), "the encoder gave no frame", false)
		}, "/dev/video4", c920WithoutAdvice},
		"a camera on the CSI connector": {softwareEncoderElements, func(h *csHarness) {
			h.s.classifyTransport = func(string) (camera.Transport, string) { return camera.TransportCSI, "tegra-capture-vi" }
		}, "/dev/video4", c920WithoutAdvice},
	} {
		t.Run(name, func(t *testing.T) {
			h := encoderCameras(t, tc.elements...)
			tc.prepare(h)
			if got := deployWarningsOf(t, h, recording(cameraSource(tc.camera))); !slices.Equal(got, []string{tc.want}) {
				t.Errorf("warnings = %q\nwant       %q", got, []string{tc.want})
			}
		})
	}

	t.Run("a device without h264parse", func(t *testing.T) {
		h := encoderCameras(t)
		h.s.gstMemo.mu.Lock()
		h.s.gstMemo.available = map[string]bool{"jpegdec": true, "x264enc": true}
		h.s.gstMemo.mu.Unlock()
		if got := deployWarningsOf(t, h, recording(cameraSource("/dev/video4"))); !slices.Equal(got, []string{c920WithoutAdvice}) {
			t.Errorf("warnings = %q\nwant       %q", got, []string{c920WithoutAdvice})
		}
	})
}

// A source that asks for a camera's own frames, as frames or as snapshot
// stills, is recorded as video where the camera hands on none, and that video
// is encoded like any other. The deploy says so in words of its own.
func TestCampaignDeployWarnings_ASourceAskingForFramesOfACameraWithoutItsOwnIsWarnedItIsRecordedAsEncodedVideo(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(h *csHarness)
		plan    encoderPlan
		want    []string
	}{
		"store frames with a buffer, only uncompressed video": {func(*csHarness) {},
			encoderPlan{buffer: "2s", sources: [][]string{cameraSource("/dev/video6", "store: frames")}}, []string{"camera v4l2:/dev/video6" + noFramesOfItsOwn}},
		"snapshot stills, only uncompressed video": {func(*csHarness) {},
			recording(cameraSource("/dev/video6", "mode: snapshot", "interval: 1s")), []string{"camera v4l2:/dev/video6" + noFramesOfItsOwn}},
		"store frames, derived frames failed recently": {func(h *csHarness) {
			h.s.derivedFailures.remember(h.s.cameraIdentityKey("/dev/video4"), "the encoder gave no frame", false)
		}, recording(cameraSource("/dev/video4", "store: frames")), []string{"camera v4l2:/dev/video4" + noFramesOfItsOwn}},
		"store frames, a camera on the CSI connector": {func(h *csHarness) {
			h.s.classifyTransport = func(string) (camera.Transport, string) { return camera.TransportCSI, "tegra-capture-vi" }
		}, recording(cameraSource("/dev/video4", "store: frames")), []string{"camera v4l2:/dev/video4" + noFramesOfItsOwn}},
		"store frames, every camera": {func(*csHarness) {},
			recording(cameraSource("*", "store: frames")), []string{"camera v4l2:/dev/video6" + noFramesOfItsOwn}},
	} {
		t.Run(name, func(t *testing.T) {
			h := encoderCameras(t, softwareEncoderElements...)
			tc.prepare(h)
			if got := deployWarningsOf(t, h, tc.plan); !slices.Equal(got, tc.want) {
				t.Errorf("warnings = %q\nwant       %q", got, tc.want)
			}
		})
	}
	t.Run("a hardware encoder", func(t *testing.T) {
		h := encoderCameras(t, "jpegdec", "v4l2h264enc", "x264enc")
		if got := deployWarningsOf(t, h, recording(cameraSource("/dev/video6", "store: frames"))); len(got) != 0 {
			t.Errorf("warnings = %q, want none", got)
		}
	})
}

// A campaign's inference reads every camera source as video for as long as it
// runs, whatever the source stores. The warning says so, and does not advise
// store: frames, which would stop nothing.
func TestCampaignDeployWarnings_AnInferenceCampaignIsWarnedOfTheEncoderItsInferenceRuns(t *testing.T) {
	inferring := func(sources ...[]string) encoderPlan { return encoderPlan{sources: sources, inference: true} }
	for name, plan := range map[string]encoderPlan{
		"store frames":    inferring(cameraSource("/dev/video4", "store: frames")),
		"snapshot stills": inferring(cameraSource("/dev/video4", "mode: snapshot", "interval: 1s")),
		"store absent":    inferring(cameraSource("/dev/video4")),
		"with a buffer":   {buffer: "2s", inference: true, sources: [][]string{cameraSource("/dev/video4", "store: both")}},
	} {
		t.Run(name, func(t *testing.T) {
			h := encoderCameras(t, softwareEncoderElements...)
			if got := deployWarningsOf(t, h, plan); !slices.Equal(got, []string{c920InferenceWarning}) {
				t.Errorf("warnings = %q\nwant       %q", got, []string{c920InferenceWarning})
			}
		})
	}
	t.Run("a camera that produces H.264", func(t *testing.T) {
		h := encoderCameras(t, softwareEncoderElements...)
		if got := deployWarningsOf(t, h, inferring(cameraSource("/dev/video2", "store: frames"))); len(got) != 0 {
			t.Errorf("warnings = %q, want none", got)
		}
	})
	t.Run("a hardware encoder", func(t *testing.T) {
		h := encoderCameras(t, "jpegdec", "v4l2h264enc", "x264enc")
		if got := deployWarningsOf(t, h, inferring(cameraSource("*"))); len(got) != 0 {
			t.Errorf("warnings = %q, want none", got)
		}
	})
}

// countingListings counts the camera listings the harness's device is asked
// for from now on.
func countingListings(h *csHarness) (listings func() int) {
	var mu sync.Mutex
	n := 0
	list := h.s.globDevices
	h.s.globDevices = func() ([]string, error) {
		mu.Lock()
		n++
		mu.Unlock()
		return list()
	}
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// A deploy lists the cameras once for a plan with a camera source, however
// many it names and whatever they store, and not at all for a plan without
// one.
func TestCampaignDeploy_ListsTheCamerasOnceForAPlanWithACameraSourceAndNeverWithout(t *testing.T) {
	for name, tc := range map[string]struct {
		plan     encoderPlan
		listings int
	}{
		"no camera source":           {encoderPlan{telemetry: true}, 0},
		"only store frames":          {recording(cameraSource("/dev/video4", "store: frames"), cameraSource("/dev/video2", "store: frames")), 1},
		"only snapshot stills":       {recording(cameraSource("*", "mode: snapshot", "interval: 1s")), 1},
		"one source stores video":    {recording(cameraSource("/dev/video4", "store: frames"), cameraSource("/dev/video2")), 1},
		"a selector names no camera": {recording(cameraSource("/dev/video4"), cameraSource("/dev/video9"), cameraSource("/dev/video2")), 1},
	} {
		t.Run(name, func(t *testing.T) {
			h := encoderCameras(t, softwareEncoderElements...)
			listings := countingListings(h)
			deployedOn(t, h, tc.plan)
			if got := listings(); got != tc.listings {
				t.Errorf("the deploy listed the cameras %d times, want %d", got, tc.listings)
			}
		})
	}
}

// hangingAdvisor is a camera adapter that arms as fakeArmingAdapter does and
// whose word on a deploy never comes until release is closed.
type hangingAdvisor struct {
	fakeArmingAdapter
	asked   chan struct{}
	release chan struct{}
}

func (a *hangingAdvisor) Discover(context.Context) []data.Source {
	return []data.Source{{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true, Detail: "front"}}
}

func (a *hangingAdvisor) DeployWarnings(context.Context, data.Campaign) []string {
	a.asked <- struct{}{}
	<-a.release
	return []string{"said too late"}
}

// What the adapters say of a deploy is a courtesy with a deadline: a listing
// that hangs holds neither the lock deploys take nor the arming of the stored
// plan, and the deploy answers without the warning.
func TestCampaignDeploy_AHungDeviceQuestionDelaysNoArmingAndYieldsNoWarning(t *testing.T) {
	budget := campaignDeployWarningBudget
	campaignDeployWarningBudget = 300 * time.Millisecond
	t.Cleanup(func() { campaignDeployWarningBudget = budget })

	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	advisor := &hangingAdvisor{asked: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(func() { close(advisor.release) })
	service.addAdapter(advisor)
	service.armingAdapter = advisor

	type answer struct {
		campaign *agentpbv2.DataCampaign
		err      error
	}
	answered := make(chan answer, 1)
	start := time.Now()
	go func() {
		campaign, err := service.CampaignDeploy(context.Background(),
			&agentpbv2.DataCampaignDeployRequest{CampaignYaml: encoderPlan{buffer: "2s", sources: [][]string{cameraSource("/dev/video0")}}.yaml()})
		answered <- answer{campaign, err}
	}()
	select {
	case <-advisor.asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the deploy never asked the adapter")
	}
	if !service.deploymentMu.TryLock() {
		t.Fatal("the deploy holds its lock while the adapter's answer is awaited")
	}
	service.deploymentMu.Unlock()
	if !csWaitFor(5*time.Second, func() bool { return advisor.armCount() == 1 }) {
		t.Fatal("the stored plan was not armed while the adapter's answer was awaited")
	}
	select {
	case got := <-answered:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if warnings := got.campaign.GetWarnings(); len(warnings) != 0 {
			t.Errorf("warnings = %q, want none", warnings)
		}
		if took := time.Since(start); took < campaignDeployWarningBudget || took > campaignDeployWarningBudget+3*time.Second {
			t.Errorf("the deploy answered after %s, want it to wait the %s it gives the adapter and no longer", took, campaignDeployWarningBudget)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the deploy never answered")
	}
}

// Once the deploy has stopped waiting, the adapter asks no further camera
// what encodes its video.
func TestCampaignDeployWarnings_AnAdapterNobodyWaitsForStopsBetweenCameras(t *testing.T) {
	h := encoderCameras(t, softwareEncoderElements...)
	campaign, err := data.ParseCampaign(recording(cameraSource("*")).yaml())
	if err != nil {
		t.Fatal(err)
	}
	adapter := newCameraDataAdapter(h.s).(dataDeployAdvisingAdapter)
	if got := adapter.DeployWarnings(context.Background(), campaign); len(got) != 2 {
		t.Fatalf("while the deploy waits: warnings = %q, want one for each of two cameras", got)
	}
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if got := adapter.DeployWarnings(done, campaign); len(got) != 0 {
		t.Errorf("after the deploy stopped waiting: warnings = %q, want none", got)
	}
}

// The warning names the preset the camera's recording states, where the
// encoder takes one: the camera has one encoder for all its consumers, so
// another may state a faster one. A campaign's inference states none.
func TestCampaignDeployWarnings_NameThePresetTheSoftwareEncoderWillRunAt(t *testing.T) {
	const (
		atVeryfast = ", at preset veryfast or a faster one another consumer of the camera states,"
		recorded   = "the campaign records or buffers; store: frames keeps the camera's own frames without encoding"
	)
	for name, tc := range map[string]struct {
		elements []string
		plan     encoderPlan
		want     string
	}{
		"the recording states a preset": {softwareEncoderElements, recording(cameraSource("/dev/video4", "store: video", "preset: veryfast")),
			"camera v4l2:/dev/video4: its video is encoded in software (x264enc)" + atVeryfast + " on this device while " + recorded},
		"store both states a preset": {softwareEncoderElements, recording(cameraSource("/dev/video4", "store: both", "preset: veryfast")),
			"camera v4l2:/dev/video4: its video is encoded in software (x264enc)" + atVeryfast + " on this device while " + recorded},
		"a buffered camera states it through its ring": {softwareEncoderElements, encoderPlan{buffer: "2s", sources: [][]string{cameraSource("/dev/video4", "preset: veryfast")}},
			"camera v4l2:/dev/video4: its video is encoded in software (x264enc)" + atVeryfast + " on this device while " + recorded},
		"the recording states none": {softwareEncoderElements, recording(cameraSource("/dev/video4", "store: video")), c920RecordedWarning},
		"the campaign's inference states none": {softwareEncoderElements, encoderPlan{inference: true, sources: [][]string{cameraSource("/dev/video4", "preset: veryfast")}},
			c920InferenceWarning},
		"a software encoder that takes no preset": {[]string{"avdec_h264", "jpegdec", "openh264enc"}, recording(cameraSource("/dev/video4", "preset: veryfast")),
			"camera v4l2:/dev/video4: its video is encoded in software (openh264enc) on this device while " + recorded},
	} {
		t.Run(name, func(t *testing.T) {
			h := encoderCameras(t, tc.elements...)
			if got := deployWarningsOf(t, h, tc.plan); !slices.Equal(got, []string{tc.want}) {
				t.Errorf("warnings = %q\nwant       %q", got, []string{tc.want})
			}
		})
	}
}
