//go:build android

package mobile

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/androidhost"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
)

func newVPNEngine(config vpn.Config) (*vpn.Engine, error) {
	return vpn.NewEngineWithDNSFallback(config, func(ctx context.Context, wire []byte) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		timeout := 10 * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			timeout = time.Until(deadline)
		}
		var answer []byte
		err := androidhost.Call("dns.query", map[string]any{"packet": wire, "timeoutMs": max(1, timeout.Milliseconds())}, &answer)
		return answer, err
	})
}
