package btradio

import (
	"context"
	"fmt"
)

// Serialise property exports, registration and refresh signals across owners.
// Waiting is cancellable so a stopped carrier cannot be kept alive by an RPC.
var advertisementGate = make(chan struct{}, 1)

func WithAdvertisement(ctx context.Context, apply func() error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case advertisementGate <- struct{}{}:
	}
	defer func() { <-advertisementGate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return apply()
}
func registerAdvertisement[T any](properties map[string]T, export func(map[string]T) error, register func() error) error {
	if err := export(properties); err != nil {
		return err
	}
	if err := register(); err != nil {
		_, min := properties["MinInterval"]
		_, max := properties["MaxInterval"]
		if !min && !max {
			return err
		}
		// Intervals are optional hints. Explicit TxPower remains in both attempts.
		retry := make(map[string]T, len(properties))
		for k, v := range properties {
			if k != "MinInterval" && k != "MaxInterval" {
				retry[k] = v
			}
		}
		if e := export(retry); e != nil {
			return fmt.Errorf("register advertisement: %w; fallback export: %v", err, e)
		}
		if e := register(); e != nil {
			return fmt.Errorf("register advertisement: %w; fallback: %v", err, e)
		}
	}
	return nil
}

// WithAdvertisementSnapshot takes the payload snapshot after obtaining the
// effect gate, so a queued power change cannot overwrite a newer refresh.
func WithAdvertisementSnapshot[T any](ctx context.Context, snapshot func() (T, error), apply func(T) error) error {
	return WithAdvertisement(ctx, func() error {
		value, err := snapshot()
		if err != nil {
			return err
		}
		return apply(value)
	})
}
