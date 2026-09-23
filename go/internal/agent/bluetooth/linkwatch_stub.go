//go:build !linux

package bluetooth

import "go.uber.org/zap"

// newLinkPlatform has nothing to watch off Linux, so Watcher.Run returns
// immediately.
func newLinkPlatform(*zap.Logger) linkPlatform { return linkPlatform{} }
