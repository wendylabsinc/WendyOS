//go:build !windows

package commands

import "github.com/wendylabsinc/wendy/go/internal/cli/qdl"

func prepareDragonwingHost(qdl.DeviceInfo) error { return nil }
