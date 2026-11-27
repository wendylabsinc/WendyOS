//go:build android

package scan

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/shared/androidhost"
	"time"
)

type androidScanner struct{ id int64 }

func RunBLECheck() int {
	if androidhost.Call("ble.check", nil, nil) != nil {
		return 1
	}
	return 0
}
func newScanner(ctx context.Context, services []string) (scanner, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var id int64
	if err := androidhost.Call("ble.scanStart", map[string]any{"services": services}, &id); err != nil {
		return nil, err
	}
	return &androidScanner{id: id}, nil
}
func (s *androidScanner) Snapshot(ctx context.Context) ([]BLEDeviceInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows []struct {
		Address, Name string
		Services      []string
		Data          map[string][]byte
		RSSI          int
		Seen          int64
	}
	if err := androidhost.Call("ble.scanSnapshot", map[string]any{"id": s.id}, &rows); err != nil {
		return nil, err
	}
	out := make([]BLEDeviceInfo, 0, len(rows))
	for _, r := range rows {
		seen := time.UnixMilli(r.Seen)
		out = append(out, BLEDeviceInfo{Address: r.Address, Name: r.Name, AdvertisementName: r.Name, AdvertisementNameSeen: seen, LastSeen: seen, ServiceDataSeen: seen, ServiceData: r.Data, ServiceUUIDs: r.Services, RSSI: r.RSSI})
	}
	return out, nil
}
func (s *androidScanner) Close() {
	_ = androidhost.Call("ble.scanStop", map[string]any{"id": s.id}, nil)
}
