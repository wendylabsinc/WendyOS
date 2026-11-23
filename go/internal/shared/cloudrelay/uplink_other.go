//go:build !linux || android

package cloudrelay

import "context"

// Android apps cannot enumerate Linux routes or bind sockets to arbitrary
// interfaces. Use the system/mobile transport, as on Apple platforms.
const interfaceFallbackSupported = false

func availableUplinks(context.Context) ([]uplink, error) { return nil, nil }
