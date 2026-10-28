//go:build !linux

package cloudrelay

import "context"

const interfaceFallbackSupported = false

func availableUplinks(context.Context) ([]uplink, error) { return nil, nil }
