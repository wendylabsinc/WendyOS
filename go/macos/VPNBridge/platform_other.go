//go:build !ios

package main

import "github.com/wendylabsinc/wendy/go/macos/vpn"

func platformConfig(cfg vpn.Config) (vpn.Config, error) { return cfg, nil }
func stopPlatform()                                     {}
