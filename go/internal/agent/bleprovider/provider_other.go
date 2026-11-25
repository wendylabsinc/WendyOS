//go:build !linux && !darwin

package bleprovider

import (
	"context"
	"errors"
)

func Run(context.Context, Config) error {
	return errors.New("BLE wendy net carrier requires Linux and BlueZ")
}
