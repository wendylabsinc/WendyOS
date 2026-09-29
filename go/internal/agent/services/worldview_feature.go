package services

import "github.com/wendylabsinc/wendy/go/internal/agent/worldview"

// worldViewFeature is the featureset flag a client reads before offering world
// view. It is a capability contract: the agent can run the managed perception
// worker on this platform, because a pinned uv artifact exists for it. Whether
// the runtime has been downloaded yet, and whether a depth camera is attached,
// are separate questions (the hardware list's camera "depth" property answers
// the second).
const worldViewFeature = "world-view"

// worldViewFeatureSupported is behind a var so tests can decide the answer
// without depending on the architecture they run on.
var worldViewFeatureSupported = worldview.Supported

func appendWorldViewFeature(features []string, supported bool) []string {
	if supported {
		return append(features, worldViewFeature)
	}
	return features
}
