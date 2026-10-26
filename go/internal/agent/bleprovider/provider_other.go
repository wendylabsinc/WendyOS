//go:build !linux

package bleprovider

import (
	"context"
	"errors"
)

func Run(context.Context, Config) error {
	return errors.New("BLE wendy net carrier requires Linux and BlueZ")
}
