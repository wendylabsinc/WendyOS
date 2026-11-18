package services

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

// inferenceShortfall is the share of the requested frames below which a model
// counts as missing its rate. Above it the rate is met near enough: the
// scoring loop polls every 20 ms, which alone costs a fast model about a
// fifth of 30 frames a second, and less of a lower rate.
const inferenceShortfall = 0.8

// inferenceThroughputHold is how long a model must miss its rate before the
// campaign says so, and how long it must keep up before the campaign takes
// that back. The process reports over its last 30 s about every 10 s, so one
// slow frame, or a slow first one, changes nothing. Behind a var for tests.
var inferenceThroughputHold = 30 * time.Second

// inferencePace is whether a campaign's model keeps up with its rate.
type inferencePace struct {
	tooSlow bool
	// since is when the reports began to disagree with tooSlow, without a
	// break. It is zero while they agree with it.
	since time.Time
}

// judge takes one report of the model process's throughput. It returns the
// comparison with the campaign's rate, or nil when nothing was scored lately,
// and whether the model has just started or stopped being too slow.
//
// The model is too slow when it scores fewer frames than the rate asks for
// and one frame takes it longer than the rate allows. A model with time to
// spare that scores too few frames is waiting for its cameras, which is not
// the model's doing and is not flagged.
func (p *inferencePace) judge(now time.Time, rate float64, cameras int, stats *data.InferenceStats, accelerator *data.AcceleratorStatus) (*data.InferenceThroughput, bool) {
	if stats == nil || stats.FPS <= 0 || stats.InferMsP50 <= 0 || cameras == 0 {
		p.since = time.Time{}
		return nil, false
	}
	// One model scores the cameras in turn, each at up to rate.
	requested := math.Round(rate*float64(cameras)*1000) / 1000
	tooSlow := stats.FPS < inferenceShortfall*requested && stats.InferMsP50*requested > 1000
	changed := false
	switch {
	case tensorRTOnItsWay(accelerator), tooSlow == p.tooSlow:
		// Nothing to judge, or nothing to change. The CPU only stands in
		// until the TensorRT engine is ready, so its speed is shown as it is.
		p.since = time.Time{}
	case p.since.IsZero():
		p.since = now
	case now.Sub(p.since) >= inferenceThroughputHold:
		p.tooSlow, p.since, changed = tooSlow, time.Time{}, true
	}

	frame := "about " + roughly(stats.InferMsP50) + " ms per frame"
	if stats.InferMsP50 >= 1000 {
		frame = "about " + roughly(stats.InferMsP50/1000) + " s per frame"
	}
	if accelerator != nil && accelerator.Active != "" {
		frame += " on " + accelerator.Active
	}
	if cameras > 1 {
		frame = fmt.Sprintf("%s for each of %d cameras; %s", strconv.FormatFloat(rate, 'f', -1, 64), cameras, frame)
	}
	return &data.InferenceThroughput{RequestedFPS: requested, AchievedFPS: stats.FPS, ModelTooSlow: p.tooSlow,
		Summary: fmt.Sprintf("model scores %s frames/s of the %s requested (%s)", roughly(stats.FPS), strconv.FormatFloat(requested, 'f', -1, 64), frame)}, changed
}

// tensorRTOnItsWay reports an engine that is offered, not yet scoring, and
// not given up on: it is being found, built or checked.
func tensorRTOnItsWay(accelerator *data.AcceleratorStatus) bool {
	return accelerator != nil && accelerator.Active != data.AcceleratorTensorRT && !accelerator.Degraded &&
		slices.Contains(accelerator.Candidates, data.AcceleratorTensorRT)
}

// roughly prints a measurement to two significant digits: 0.11, 9.1, 212.
func roughly(value float64) string {
	text := strconv.FormatFloat(value, 'g', 2, 64)
	if strings.Contains(text, "e") {
		text = strconv.FormatFloat(value, 'f', 0, 64)
	}
	return text
}

// throughputState compares the latest throughput report with the campaign's
// rate, for inspect. It returns the line to log when the model has just
// started to miss the rate, or to keep up with it again, and "" otherwise.
// Callers hold j.mu, and log without it.
func (j *campaignInferenceJob) throughputState(now time.Time) string {
	cameras := 0
	for _, state := range j.status.Sources {
		// The state a scored frame leaves its camera in (run).
		if state == "detecting" || state == "classifying" {
			cameras++
		}
	}
	throughput, changed := j.pace.judge(now, j.campaign.Inference.Rate, cameras, j.status.Stats, j.status.Accelerator)
	j.status.Throughput = throughput
	switch {
	case !changed:
		return ""
	case throughput.ModelTooSlow:
		return fmt.Sprintf("campaign %q inference cannot keep up with its rate: %s", j.campaign.Name, throughput.Summary)
	default:
		return fmt.Sprintf("campaign %q inference keeps up with its rate again: %s", j.campaign.Name, throughput.Summary)
	}
}
