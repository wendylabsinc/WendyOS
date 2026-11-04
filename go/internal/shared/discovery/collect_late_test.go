package discovery

import (
	"context"
	"testing"
	"time"
)

// A scan timeout is a normal conclusion, not caller cancellation. Confirmed
// answers late in the window must survive even while the settle timer is pending.
func TestCollectLANKeepsLateAnswerAtScanTimeout(t *testing.T) {
	useStreamSeams(t, func(ctx context.Context, _ string, emit func(MDNSService)) error {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(70 * time.Millisecond):
		}
		emit(MDNSService{Hostname: "late.local", IPAddress: "192.0.2.5", Port: 50052, TXTRecords: map[string]string{"id": "late", "tls": "true", "assetid": "675", "orgid": "64"}})
		<-ctx.Done()
		return nil
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	found, err := CollectLAN(ctx, StreamOptions{}, 150*time.Millisecond)
	if err != nil || len(found) != 1 || found[0].AssetID != 675 {
		t.Fatalf("late result lost: %+v %v", found, err)
	}
	if ctx.Err() != nil {
		t.Fatal("scan consumed caller deadline")
	}
}
