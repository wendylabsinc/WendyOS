package litetopology

import (
	"context"
	"errors"
	lite "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"testing"
	"time"
)

func TestSetupTimerStopsWithoutCancelingEstablishedLifetime(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	ctx, finish, cancel := BeginSetup(parent, 20*time.Millisecond)
	defer cancel()
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("successful setup canceled stream")
	case <-time.After(40 * time.Millisecond):
	}
	cancelParent()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("caller cancellation lost after setup")
	}
	ctx, finish, cancel = BeginSetup(context.Background(), 10*time.Millisecond)
	defer cancel()
	<-ctx.Done()
	if !errors.Is(finish(), context.DeadlineExceeded) {
		t.Fatal("setup ceiling lost")
	}
}
func TestSixtyFourPagedOriginsHaveIndependentAggregateBudget(t *testing.T) {
	calls := 0
	snapshot, err := readWithin(context.Background(), func(ctx context.Context, q *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Millisecond):
		}
		calls++
		page := fixture(t)
		page.Record.Kind = 3
		page.Record.Authorized = true
		page.Record.Revision = uint64(calls)
		page.NextCursor = q.Cursor + 1
		if calls == 64 {
			page.NextCursor = 0
		}
		return page, nil
	}, 500*time.Millisecond)
	if err != nil || calls != 64 || len(snapshot.Announcements) != 64 {
		t.Fatal(calls, snapshot, err)
	}
	calls = 0
	snapshot, err = readWithin(context.Background(), func(ctx context.Context, q *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		calls++
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
		page := fixture(t)
		page.NextCursor = q.Cursor + 1
		return page, nil
	}, 35*time.Millisecond)
	if snapshot != nil || !errors.Is(err, context.DeadlineExceeded) || calls >= 64 {
		t.Fatal(calls, snapshot, err)
	}
}

func TestCompletedSetupAtTimerBoundaryCannotCancelLater(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		ctx, finish, cancel := BeginSetup(context.Background(), 0)
		if finish() == nil {
			time.Sleep(time.Millisecond)
			if ctx.Err() != nil {
				t.Fatal("setup completion raced a late timer callback")
			}
		}
		cancel()
	}
}
