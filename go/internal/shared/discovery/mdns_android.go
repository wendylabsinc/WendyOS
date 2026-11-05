//go:build android

package discovery

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/shared/androidhost"
	"time"
)

func mdnsStreamBackend(ctx context.Context, serviceType string, emit func(MDNSService)) error {
	var id int64
	if err := androidhost.Call("mdns.start", map[string]any{"type": serviceType}, &id); err != nil {
		return err
	}
	defer androidhost.Call("mdns.stop", map[string]any{"id": id}, nil)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		var rows []MDNSService
		if err := androidhost.Call("mdns.snapshot", map[string]any{"id": id}, &rows); err != nil {
			return err
		}
		for _, row := range rows {
			emit(row)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
