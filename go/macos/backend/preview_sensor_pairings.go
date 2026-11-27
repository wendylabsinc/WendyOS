package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// PreviewSensorPairings reads saved source names over the preview's connection.
// It neither scans the network nor changes the interactive device or pairings.
func (b *Backend) PreviewSensorPairings(id uint64) (commands.DesktopPairings, error) {
	p := b.preview(id)
	if p == nil {
		return commands.DesktopPairings{}, errors.New("camera preview is closed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ctx.Err(); err != nil {
		return commands.DesktopPairings{}, err
	}
	ctx, conn, done, err := p.backend.deviceRPC(p.backend.session)
	if err != nil {
		return commands.DesktopPairings{}, err
	}
	defer done()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	r, err := conn.SensorPairingService.ListSensorPairings(ctx, &pb.ListSensorPairingsRequest{})
	if err != nil {
		return commands.DesktopPairings{}, err
	}
	out := commands.DesktopPairings{}
	for _, source := range r.GetPairings() {
		id := source.GetSourcePrincipalUri()
		if id == "" && source.GetSourceAssetId() > 0 {
			id = fmt.Sprintf("asset:%d", source.GetSourceAssetId())
		}
		name := strings.TrimSpace(source.GetName())
		if id != "" && name != "" {
			out.Items = append(out.Items, commands.DesktopPairing{ID: id, Name: name, Paired: true, Connected: source.GetConnected()})
		}
	}
	return out, nil
}
