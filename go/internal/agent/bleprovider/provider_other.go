//go:build !linux

package bleprovider

import (
	"context"
	"errors"
)

func Run(context.Context, Config) error {
	return errors.New("BLE local-mesh carrier requires Linux and BlueZ")
}
